// Package httpdl downloads plain files, splitting them into byte ranges that
// are fetched in parallel when the server supports it.
package httpdl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/503stack/trawl/internal/fetch"
)

// ErrPlaylist means the URL turned out to be an HLS playlist.
var ErrPlaylist = errors.New("response is an HLS playlist")

// Result describes a finished download.
type Result struct {
	Size        int64
	Filename    string // from Content-Disposition
	ContentType string
	FinalURL    *url.URL
	// Connections is how many parallel connections were left after the
	// server turned the others away; 0 if it turned none away.
	Connections int
}

// Downloader downloads one URL.
type Downloader struct {
	Client    *fetch.Client
	Header    http.Header
	Workers   int
	ChunkSize int64
}

// Download fetches rawURL into dst. prog receives bytes done and the total
// size (-1 if unknown).
func (d *Downloader) Download(ctx context.Context, rawURL, dst string, prog func(done, total int64)) (*Result, error) {
	if err := d.Client.Acquire(ctx); err != nil {
		return nil, err
	}
	var (
		resp *http.Response
		body *fetch.Body
	)
	// "bytes=0-" asks for everything, but a 206 answer tells us ranges work.
	err := d.Client.Retry(ctx, func() (err error) {
		resp, body, err = d.Client.Get(ctx, rawURL, d.Header, "bytes=0-")
		return err
	})
	if err != nil {
		d.Client.Release()
		return nil, err
	}
	res := &Result{
		ContentType: resp.Header.Get("Content-Type"),
		FinalURL:    resp.Request.URL,
		Filename:    dispositionName(resp.Header.Get("Content-Disposition")),
	}
	if strings.Contains(strings.ToLower(res.ContentType), "mpegurl") {
		body.Close()
		d.Client.Release()
		return res, ErrPlaylist
	}
	total := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent {
		total = rangeTotal(resp.Header.Get("Content-Range"))
	}
	if resp.StatusCode == http.StatusPartialContent && total > 2*d.ChunkSize && d.Workers > 1 {
		body.Close()
		d.Client.Release()
		res.Size = total
		res.Connections, err = d.parallel(ctx, res.FinalURL.String(), dst, total, prog)
		return res, err
	}

	defer d.Client.Release()
	defer body.Close()
	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var done int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return nil, err
			}
			done += int64(n)
			prog(done, total)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if total >= 0 && done != total {
		return nil, fmt.Errorf("incomplete: %d of %d bytes", done, total)
	}
	res.Size = done
	return res, f.Close()
}

// span is a byte range still to fetch, inclusive.
type span struct{ start, end int64 }

// work is the queue of spans shared by the workers. A worker that the server
// turns away (503, 429) while others are still running gives its remaining
// span back and stops, so a job settles at the number of connections the
// server allows. The last worker never stops this way; it retries instead.
type work struct {
	mu     sync.Mutex
	queue  []span
	active int
	shed   int
}

func (w *work) pop() (span, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		w.active--
		return span{}, false
	}
	s := w.queue[0]
	w.queue = w.queue[1:]
	return s, true
}

// giveBack requeues s and retires the calling worker, unless it is the last.
func (w *work) giveBack(s span) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active <= 1 {
		return false
	}
	w.active--
	w.shed++
	w.queue = append(w.queue, s)
	return true
}

var errShed = errors.New("connection refused by server; span handed back")

func (d *Downloader) parallel(ctx context.Context, u, dst string, total int64, prog func(done, total int64)) (int, error) {
	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := f.Truncate(total); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w := &work{}
	for start := int64(0); start < total; start += d.ChunkSize {
		w.queue = append(w.queue, span{start, min(start+d.ChunkSize, total) - 1})
	}
	workers := min(d.Workers, len(w.queue))
	w.active = workers
	var (
		done     atomic.Int64
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				s, ok := w.pop()
				if !ok {
					return
				}
				err := d.chunk(ctx, u, f, s, w, func(n int64) { prog(done.Add(n), total) })
				if errors.Is(err, errShed) {
					return
				}
				if err != nil {
					once.Do(func() {
						firstErr = fmt.Errorf("bytes %d-%d: %w", s.start, s.end, err)
						cancel()
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return 0, firstErr
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if got := done.Load(); got != total {
		return 0, fmt.Errorf("incomplete: %d of %d bytes", got, total)
	}
	if w.shed == 0 {
		return 0, f.Close()
	}
	return workers - w.shed, f.Close()
}

// chunk downloads span s, resuming from where it got to on each retry. When
// the server refuses the connection (503, 429) and other workers are still
// running, it hands the rest of the span back to w and returns errShed.
func (d *Downloader) chunk(ctx context.Context, u string, f *os.File, s span, w *work, add func(int64)) error {
	pos, end := s.start, s.end
	buf := make([]byte, 256<<10)
	return d.Client.Retry(ctx, func() error {
		if err := d.Client.Acquire(ctx); err != nil {
			return err
		}
		defer d.Client.Release()
		resp, body, err := d.Client.Get(ctx, u, d.Header, fmt.Sprintf("bytes=%d-%d", pos, end))
		if err != nil {
			var se *fetch.StatusError
			if errors.As(err, &se) && (se.Code == http.StatusServiceUnavailable || se.Code == http.StatusTooManyRequests) &&
				w.giveBack(span{pos, end}) {
				return fetch.Permanent(errShed)
			}
			return err
		}
		defer body.Close()
		if resp.StatusCode != http.StatusPartialContent || rangeStart(resp.Header.Get("Content-Range")) != pos {
			return fetch.Permanent(fmt.Errorf("server stopped honouring range requests (HTTP %d)", resp.StatusCode))
		}
		for pos <= end {
			n, rerr := body.Read(buf)
			if n > 0 {
				n = int(min(int64(n), end-pos+1))
				if _, err := f.WriteAt(buf[:n], pos); err != nil {
					return fetch.Permanent(err)
				}
				pos += int64(n)
				add(int64(n))
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return rerr
			}
		}
		if pos <= end {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
}

// dispositionName returns the filename from a Content-Disposition header.
func dispositionName(h string) string {
	if h == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(h)
	if err != nil {
		return ""
	}
	return path.Base(strings.ReplaceAll(params["filename"], `\`, "/"))
}

// "bytes 0-99/1234" → 1234; unknown → -1.
func rangeTotal(h string) int64 {
	_, total, ok := strings.Cut(h, "/")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// "bytes 100-199/1234" → 100; unknown → -1.
func rangeStart(h string) int64 {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "bytes"))
	start, _, ok := strings.Cut(s, "-")
	if !ok {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	if err != nil {
		return -1
	}
	return n
}
