package hls

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"sync"

	"github.com/503stack/trawl/internal/fetch"
)

const (
	maxPlaylist = 16 << 20
	maxSegment  = 256 << 20
)

// Plan is what to download for one HLS job.
type Plan struct {
	Video  *Media
	Audio  *Media // separate audio rendition, if any
	Height int    // expected video height, 0 if unknown
	Source string // media playlist actually used
	Notes  []string
}

var qualityName = regexp.MustCompile(`^(\d{3,4})p\.m3u8$`)

// Load fetches a playlist, picks the best variant and loads its media
// playlists. With upgrade set, a quality-named playlist such as 360p.m3u8 is
// swapped for a better sibling (master.m3u8, 720p.m3u8, ...) when one exists.
func Load(ctx context.Context, c *fetch.Client, h http.Header, rawURL string, upgrade bool) (*Plan, error) {
	data, final, err := c.Fetch(ctx, rawURL, h, maxPlaylist)
	if err != nil {
		return nil, fmt.Errorf("playlist: %w", err)
	}
	master, media, err := Parse(data, final)
	if err != nil {
		return nil, err
	}
	p := &Plan{}
	if media != nil && upgrade {
		if better, from, to := findUpgrade(ctx, c, h, final); better != "" {
			up, err := Load(ctx, c, h, better, false)
			if err == nil {
				up.Notes = append([]string{fmt.Sprintf("upgraded %dp → %s", from, to)}, up.Notes...)
				return up, nil
			}
		}
	}
	if media != nil {
		p.Video, p.Source = media, final.String()
		if m := qualityName.FindStringSubmatch(path.Base(final.Path)); m != nil {
			p.Height, _ = strconv.Atoi(m[1])
		}
	} else {
		v := master.Best()
		p.Height = v.Height
		p.Notes = append(p.Notes, fmt.Sprintf("picked best of %d variants: %s", len(master.Variants), describe(v)))
		if p.Video, p.Source, err = loadMedia(ctx, c, h, v.URI); err != nil {
			return nil, fmt.Errorf("variant playlist: %w", err)
		}
		if r := master.Audio(v.Audio); r != nil {
			if p.Audio, _, err = loadMedia(ctx, c, h, r.URI); err != nil {
				return nil, fmt.Errorf("audio playlist: %w", err)
			}
			p.Notes = append(p.Notes, "separate audio track: "+firstNonEmpty(r.Name, r.Language, r.Group))
		}
	}
	if p.Video.Live {
		p.Notes = append(p.Notes, "playlist has no end marker (live?); downloading the segments listed now")
	}
	return p, nil
}

func loadMedia(ctx context.Context, c *fetch.Client, h http.Header, u string) (*Media, string, error) {
	data, final, err := c.Fetch(ctx, u, h, maxPlaylist)
	if err != nil {
		return nil, "", err
	}
	_, media, err := Parse(data, final)
	if err == nil && media == nil {
		err = errors.New("expected a media playlist, got a master playlist")
	}
	return media, final.String(), err
}

// findUpgrade looks for a better playlist next to a quality-named one.
func findUpgrade(ctx context.Context, c *fetch.Client, h http.Header, u *url.URL) (string, int, string) {
	m := qualityName.FindStringSubmatch(path.Base(u.Path))
	if m == nil {
		return "", 0, ""
	}
	cur, _ := strconv.Atoi(m[1])
	sibling := func(name string) *url.URL { return u.ResolveReference(&url.URL{Path: name, RawQuery: u.RawQuery}) }
	for _, name := range []string{"master.m3u8", "playlist.m3u8", "index.m3u8"} {
		s := sibling(name)
		data, final, err := c.FetchOnce(ctx, s.String(), h, maxPlaylist)
		if err != nil {
			continue
		}
		if master, _, err := Parse(data, final); err == nil && master != nil {
			if v := master.Best(); v.Height > cur {
				return s.String(), cur, name + " " + describe(v)
			}
		}
	}
	for _, q := range []int{2160, 1440, 1080, 720, 480} {
		if q <= cur {
			break
		}
		s := sibling(strconv.Itoa(q) + "p.m3u8")
		data, final, err := c.FetchOnce(ctx, s.String(), h, maxPlaylist)
		if err != nil {
			continue
		}
		if _, media, err := Parse(data, final); err == nil && media != nil {
			return s.String(), cur, fmt.Sprintf("%dp", q)
		}
	}
	return "", 0, ""
}

func describe(v Variant) string {
	s := fmt.Sprintf("%d kbit/s", v.Bandwidth/1000)
	if v.Height > 0 {
		s = fmt.Sprintf("%dp, %s", v.Height, s)
	}
	return s
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// Downloader fetches segments in parallel and writes them in order.
type Downloader struct {
	Client  *fetch.Client
	Header  http.Header
	Workers int

	mu   sync.Mutex
	keys map[string][]byte
}

// Download writes all segments of m (decrypted, in order) to dst. prog is
// called after each segment with total bytes written and segments done.
func (d *Downloader) Download(ctx context.Context, m *Media, dst string, prog func(bytes int64, segs int)) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()

	n := len(m.Segments)
	workers := max(1, min(d.Workers, n))
	// At most `window` segments are in flight or buffered, which bounds memory
	// while one slow segment holds up writing.
	window := workers * 2
	slots := make(chan struct{}, window)
	jobs := make(chan int)
	type result struct {
		i    int
		data []byte
		err  error
	}
	results := make(chan result, window)

	go func() {
		defer close(jobs)
		for i := range n {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				data, err := d.segment(ctx, m.Segments[i])
				select {
				case results <- result{i, data, err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	pending := map[int][]byte{}
	next := 0
	var lastInit Part
	var written int64
	for r := range results {
		if err != nil {
			continue // drain until workers exit
		}
		if r.err != nil {
			err = fmt.Errorf("segment %d/%d: %w", r.i+1, n, r.err)
			cancel()
			continue
		}
		pending[r.i] = r.data
		for err == nil {
			data, ok := pending[next]
			if !ok {
				break
			}
			if init := m.Segments[next].Init; init != nil && *init != lastInit {
				b, ierr := d.fetchAll(ctx, *init)
				if ierr == nil {
					_, ierr = f.Write(b)
				}
				if ierr != nil {
					err = fmt.Errorf("init segment: %w", ierr)
					cancel()
					break
				}
				written += int64(len(b))
				lastInit = *init
			}
			if _, werr := f.Write(data); werr != nil {
				err = werr
				cancel()
				break
			}
			written += int64(len(data))
			delete(pending, next)
			next++
			<-slots
			prog(written, next)
		}
	}
	if err != nil {
		return err
	}
	if next != n {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("incomplete: %d of %d segments", next, n)
	}
	return nil
}

func (d *Downloader) segment(ctx context.Context, s Segment) ([]byte, error) {
	data, err := d.fetchAll(ctx, s.Part)
	if err != nil {
		return nil, err
	}
	if s.Key != nil {
		key, err := d.key(ctx, s.Key.URI)
		if err != nil {
			return nil, err
		}
		iv := s.Key.IV
		if iv == nil {
			iv = make([]byte, 16)
			binary.BigEndian.PutUint64(iv[8:], uint64(s.Seq))
		}
		if data, err = Decrypt(data, key, iv); err != nil {
			return nil, err
		}
	}
	if fetch.LooksLikeErrorPage(data) {
		return nil, errors.New("got a web page instead of video data (link expired?)")
	}
	return data, nil
}

// fetchAll downloads one resource or byte range completely, checking
// Content-Length. Servers that ignore Range get the range cut out locally.
func (d *Downloader) fetchAll(ctx context.Context, p Part) ([]byte, error) {
	var data []byte
	err := d.Client.Retry(ctx, func() error {
		if err := d.Client.Acquire(ctx); err != nil {
			return err
		}
		defer d.Client.Release()
		resp, body, err := d.Client.Get(ctx, p.URI, d.Header, p.Range())
		if err != nil {
			return err
		}
		defer body.Close()
		var src io.Reader = body
		if p.Len > 0 && resp.StatusCode == http.StatusOK {
			if _, err := io.CopyN(io.Discard, body, p.Off); err != nil {
				return err
			}
			src = io.LimitReader(body, p.Len)
			resp.ContentLength = p.Len
		}
		var buf bytes.Buffer
		if resp.ContentLength > 0 && resp.ContentLength <= maxSegment {
			buf.Grow(int(resp.ContentLength))
		}
		n, err := io.Copy(&buf, io.LimitReader(src, maxSegment))
		if err != nil {
			return err
		}
		if resp.ContentLength > 0 && n != resp.ContentLength {
			return fmt.Errorf("short read: %d of %d bytes", n, resp.ContentLength)
		}
		data = buf.Bytes()
		return nil
	})
	return data, err
}

func (d *Downloader) key(ctx context.Context, uri string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if k, ok := d.keys[uri]; ok {
		return k, nil
	}
	k, _, err := d.Client.Fetch(ctx, uri, d.Header, 1024)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	if len(k) != 16 {
		return nil, fmt.Errorf("key is %d bytes, want 16", len(k))
	}
	if d.keys == nil {
		d.keys = map[string][]byte{}
	}
	d.keys[uri] = k
	return k, nil
}

// Decrypt decrypts an AES-128-CBC segment in place and strips PKCS#7 padding.
func Decrypt(data, key, iv []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted segment size %d is not a multiple of 16", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || !bytes.Equal(data[len(data)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("bad padding after decryption (wrong key?)")
	}
	return data[:len(data)-pad], nil
}
