package queue

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/503stack/trawl/internal/hls"
	"github.com/503stack/trawl/internal/httpdl"
	"github.com/503stack/trawl/internal/media"
	"github.com/503stack/trawl/internal/naming"
	"github.com/503stack/trawl/internal/notify"
	"github.com/503stack/trawl/internal/page"
)

var errDuplicate = errors.New("duplicate")

// download is what the transfer step produced.
type download struct {
	path     string
	isHLS    bool
	names    []string // filename candidates: Content-Disposition, URL path
	ctype    string
	final    *url.URL // plain files: URL after redirects
	expected media.Expect
}

// pipeline: download → transfer checks → remux → media checks → hash →
// name/extension → dedupe → atomic move.
func (m *Manager) pipeline(ctx context.Context, j *Job, r *run) error {
	req := j.Request
	work := filepath.Join(m.cfg.TempDir, j.ID)
	os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(work)

	hdr := buildHeader(req)
	var title string
	var pg *webPage
	if req.PageURL != "" && (req.Name == "" || req.URL == "") {
		p, err := m.fetchPage(ctx, req.PageURL, hdr)
		if err != nil {
			m.note(j, "could not read page: %v", err)
		} else {
			pg = p
			title = page.Title(p.data)
		}
	}

	src := req.URL
	if src == "" {
		u, ref, err := m.pick(ctx, j, req.Candidates, pg, hdr)
		if err != nil {
			return err
		}
		src, hdr = u, withReferer(req, hdr, ref)
	}
	dl, err := m.transfer(ctx, j, r, work, hdr, src)
	if err != nil {
		return err
	}
	// The link was a web page rather than a file: look for the video in it.
	if dl.ctype == "text/html" && req.URL != "" && !looksLikeMedia(req.URL) {
		if p, err := readPage(dl); err == nil {
			if title == "" {
				title = page.Title(p.data)
			}
			if u, ref, err := m.pick(ctx, j, nil, p, hdr); err == nil {
				os.Remove(dl.path)
				r.bytes.Store(0)
				r.total.Store(0)
				h := hdr
				if req.PageURL == "" {
					h = withReferer(req, hdr, req.URL)
				}
				if dl, err = m.transfer(ctx, j, r, work, withReferer(req, h, ref), u); err != nil {
					return err
				}
			}
		}
	}

	m.update(j, func(j *Job) { j.State = Verifying })
	cands := []string{req.Name, title}
	for _, n := range dl.names {
		b, _ := naming.SplitExt(n)
		cands = append(cands, b)
	}
	base := m.namer.Pick(cands...)
	if base == "" {
		base = "trawl-" + time.Now().Format("20060102-150405")
	}
	out, ext, info, err := m.verify(ctx, j, dl)
	if err != nil {
		m.quarantine(j, dl.path, base, ext)
		return err
	}

	sum, err := media.SHA256(out)
	if err != nil {
		return err
	}
	st, err := os.Stat(out)
	if err != nil {
		return err
	}
	folder, _ := m.cfg.Folder(req.Folder)
	m.update(j, func(j *Job) { j.SHA256, j.Size, j.Media = sum, st.Size(), info })

	dedupe := req.Dedupe
	if dedupe == "" {
		dedupe = m.cfg.Dedupe
	}
	if dedupe != "off" {
		if dup := findDuplicate(folder.Path, st.Size(), sum); dup != "" {
			if dedupe == "skip" {
				// Hold the file so the user can still keep it without downloading again.
				held, err := naming.Place(out, filepath.Join(m.cfg.DataDir, "duplicates"), j.ID+m.cfg.NameSeparator+base, ext, m.cfg.NameSeparator)
				if err != nil {
					return err
				}
				m.update(j, func(j *Job) { j.DupOf, j.Held, j.HeldName = dup, held, base+ext })
				return errDuplicate
			}
			m.note(j, "identical to %s", filepath.Base(dup))
		}
	}
	final, err := naming.Place(out, folder.Path, base, ext, m.cfg.NameSeparator)
	if err != nil {
		return err
	}
	m.update(j, func(j *Job) { j.Output = final })
	return nil
}

// transfer downloads the URL as HLS or as a plain file.
func (m *Manager) transfer(ctx context.Context, j *Job, r *run, work string, hdr http.Header, src string) (*download, error) {
	u, _ := url.Parse(src)
	p := strings.ToLower(u.Path)
	if !strings.HasSuffix(p, ".m3u8") && !strings.HasSuffix(p, ".m3u") {
		m.update(j, func(j *Job) { j.Kind = "http" })
		d := &httpdl.Downloader{Client: m.client, Header: hdr, Workers: m.cfg.JobConnections, ChunkSize: m.cfg.ChunkSize}
		dst := filepath.Join(work, "download")
		res, err := d.Download(ctx, src, dst, func(done, total int64) {
			r.bytes.Store(done)
			r.total.Store(total)
		})
		if err != nil && !errors.Is(err, httpdl.ErrPlaylist) {
			return nil, err
		}
		if err == nil {
			ctype, _ := media.Sniff(dst)
			if ctype != "application/vnd.apple.mpegurl" {
				return &download{
					path:  dst,
					names: []string{res.Filename, path.Base(res.FinalURL.Path)},
					ctype: ctype,
					final: res.FinalURL,
				}, nil
			}
		}
		os.Remove(dst)
		r.bytes.Store(0)
		r.total.Store(0)
	}
	return m.transferHLS(ctx, j, r, work, hdr, src)
}

func (m *Manager) transferHLS(ctx context.Context, j *Job, r *run, work string, hdr http.Header, src string) (*download, error) {
	req := j.Request
	m.update(j, func(j *Job) { j.Kind = "hls" })
	plan, err := hls.Load(ctx, m.client, hdr, src, req.Upgrade == nil || *req.Upgrade)
	if err != nil {
		return nil, err
	}
	for _, n := range plan.Notes {
		m.note(j, "%s", n)
	}
	segs := len(plan.Video.Segments)
	if plan.Audio != nil {
		segs += len(plan.Audio.Segments)
	}
	r.segTotal.Store(int64(segs))

	d := &hls.Downloader{Client: m.client, Header: hdr, Workers: m.cfg.JobConnections}
	var baseBytes int64
	var baseSegs int
	grab := func(pl *hls.Media, name string) (string, error) {
		ext := ".ts"
		if pl.Segments[0].Init != nil {
			ext = ".mp4"
		}
		dst := filepath.Join(work, name+ext)
		var last int64
		err := d.Download(ctx, pl, dst, func(b int64, s int) {
			last = b
			r.bytes.Store(baseBytes + b)
			r.segDone.Store(int64(baseSegs + s))
		})
		baseBytes += last
		baseSegs += len(pl.Segments)
		return dst, err
	}
	video, err := grab(plan.Video, "video")
	if err != nil {
		return nil, err
	}
	inputs := []string{video}
	if plan.Audio != nil {
		audio, err := grab(plan.Audio, "audio")
		if err != nil {
			return nil, fmt.Errorf("audio: %w", err)
		}
		inputs = append(inputs, audio)
	}

	out := filepath.Join(work, "remux.mp4")
	if err := media.Remux(ctx, inputs, out); err != nil {
		mkv := filepath.Join(work, "remux.mkv")
		if err2 := media.Remux(ctx, inputs, mkv); err2 != nil {
			return nil, fmt.Errorf("remux: %w", err)
		}
		m.note(j, "streams do not fit in mp4, saved as mkv")
		out = mkv
	}
	for _, in := range inputs {
		os.Remove(in)
	}
	pu, _ := url.Parse(plan.Source)
	return &download{
		path:     out,
		isHLS:    true,
		names:    []string{path.Base(pu.Path), path.Base(strings.TrimSuffix(path.Dir(pu.Path), "/"))},
		ctype:    "video/mp4",
		expected: media.Expect{Duration: plan.Video.Duration, Height: plan.Height},
	}, nil
}

// verify checks the downloaded file and decides its extension. It may remux
// the file, so it returns the path to use from now on.
func (m *Manager) verify(ctx context.Context, j *Job, dl *download) (string, string, *media.Info, error) {
	level := j.Verify
	if level == "" {
		level = m.cfg.VerifyLevel
	}
	candExt := ""
	for _, n := range dl.names {
		if _, e := naming.SplitExt(n); e != "" && e != ".m3u8" {
			candExt = e
			break
		}
	}
	if dl.ctype == "text/html" && candExt != ".html" && candExt != ".htm" {
		return "", candExt, nil, errors.New("server sent a web page instead of the file (link expired or missing headers?)")
	}

	out, ext := dl.path, ""
	var info *media.Info
	if dl.isHLS || media.IsMedia(dl.ctype) || media.IsMediaExt(candExt) {
		var err error
		info, err = media.Probe(ctx, out)
		if err != nil && level != "transfer" {
			return "", candExt, nil, err
		}
		if info != nil && level != "transfer" {
			st, err := os.Stat(out)
			if err != nil {
				return "", "", nil, err
			}
			e := dl.expected
			e.MinSize, e.Tolerance, e.RequireAudio = m.cfg.MinSize, m.cfg.DurationTolerance, m.cfg.RequireAudio
			warnings, err := media.Check(info, st.Size(), e)
			if err != nil {
				return "", media.ExtFor(info), info, err
			}
			for _, w := range warnings {
				m.note(j, "%s", w)
			}
		}
		if level == "decode" {
			if err := media.Decode(ctx, out); err != nil {
				return "", candExt, info, err
			}
		}
		if info != nil {
			ext = media.ExtFor(info)
		}
	}
	if ext == "" {
		ext = candExt
	}
	if ext == "" {
		ext = media.ExtForType(dl.ctype)
	}
	if ruled := naming.ApplyRules(ext, m.cfg.ExtRules); ruled != ext {
		if info != nil && media.NeedsRemux(info, ruled) {
			conv := filepath.Join(filepath.Dir(out), "convert"+ruled)
			if err := media.Remux(ctx, []string{out}, conv); err != nil {
				m.note(j, "could not convert to %s, keeping %s: %v", ruled, ext, err)
				return out, ext, info, nil
			}
			os.Remove(out)
			out = conv
		}
		ext = ruled
	}
	return out, ext, info, nil
}

// quarantine keeps a file that failed verification in DATA/failed.
func (m *Manager) quarantine(j *Job, path, base, ext string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	dst, err := naming.Place(path, filepath.Join(m.cfg.DataDir, "failed"), j.ID+m.cfg.NameSeparator+base, ext, m.cfg.NameSeparator)
	if err != nil {
		m.log.Error("quarantine", "id", j.ID, "err", err)
		return
	}
	m.update(j, func(j *Job) { j.Failed = dst })
}

// findDuplicate returns a file in dir with the same size and hash, if any.
func findDuplicate(dir string, size int64, sum string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Size() != size {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if s, err := media.SHA256(p); err == nil && s == sum {
			return p
		}
	}
	return ""
}

// buildHeader parses the user's header lines and adds Referer/Origin from the
// page URL unless they were given explicitly.
func buildHeader(req Request) http.Header {
	h := http.Header{}
	for _, line := range strings.Split(req.Headers, "\n") {
		k, v, ok := strings.Cut(line, ":")
		k = strings.TrimSpace(k)
		if ok && k != "" {
			h.Add(textproto.CanonicalMIMEHeaderKey(k), strings.TrimSpace(v))
		}
	}
	if p, err := url.Parse(req.PageURL); err == nil && req.PageURL != "" {
		if h.Get("Referer") == "" {
			h.Set("Referer", req.PageURL)
		}
		if h.Get("Origin") == "" {
			h.Set("Origin", p.Scheme+"://"+p.Host)
		}
	}
	return h
}

// webPage is a fetched HTML page and the URL it came from after redirects.
type webPage struct {
	data []byte
	url  *url.URL
}

const maxPage = 4 << 20

// fetchPage reads a web page the way a browser opening it would: without the
// Referer and Origin meant for the media requests.
func (m *Manager) fetchPage(ctx context.Context, pageURL string, hdr http.Header) (*webPage, error) {
	h := hdr.Clone()
	h.Del("Origin")
	h.Del("Referer")
	data, final, err := m.client.Fetch(ctx, pageURL, h, maxPage)
	if err != nil {
		return nil, err
	}
	return &webPage{data, final}, nil
}

// readPage loads a downloaded file that turned out to be a web page.
func readPage(dl *download) (*webPage, error) {
	f, err := os.Open(dl.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPage))
	return &webPage{data, dl.final}, err
}

// pick chooses the video to download from the URLs the bookmarklet saw in the
// browser and those in the page's HTML. When the page has none, it looks in
// up to three iframes (embedded players). It returns the URL and the page it
// was found on, which becomes the Referer.
func (m *Manager) pick(ctx context.Context, j *Job, browser []string, pg *webPage, hdr http.Header) (string, string, error) {
	type cand struct{ url, ref string }
	var list []cand
	add := func(urls []string, ref string) {
		for _, u := range urls {
			list = append(list, cand{u, ref})
		}
	}
	add(page.Clean(browser, nil), "")
	if pg != nil {
		add(page.Find(pg.data, pg.url), "")
		if len(list) == 0 {
			frames := page.Iframes(pg.data, pg.url)
			for _, f := range frames[:min(len(frames), 3)] {
				h := hdr.Clone()
				h.Set("Referer", pg.url.String())
				h.Del("Origin")
				data, final, err := m.client.Fetch(ctx, f, h, maxPage)
				if err != nil {
					m.note(j, "could not read embedded player %s: %v", f, err)
					continue
				}
				add(page.Find(data, final), final.String())
			}
		}
	}
	if len(list) == 0 {
		return "", "", errors.New("no video or playlist found on the page (the player may build its URL in JavaScript: use the bookmarklet while the video plays, or paste the .m3u8 link)")
	}
	ref := map[string]string{}
	var urls []string
	for _, c := range list {
		if _, ok := ref[c.url]; !ok {
			ref[c.url] = c.ref
			urls = append(urls, c.url)
		}
	}
	urls = page.Best(urls)
	m.update(j, func(j *Job) { j.Found = urls[0] })
	m.note(j, "found on page: %s", urls[0])
	if n := len(urls) - 1; n > 0 {
		m.note(j, "%d other candidate(s), use Retry with new URL to pick one: %s", n, strings.Join(urls[1:min(len(urls), 4)], " "))
	}
	return urls[0], ref[urls[0]], nil
}

// withReferer makes ref the Referer and Origin unless the user set them.
func withReferer(req Request, hdr http.Header, ref string) http.Header {
	if ref == "" {
		return hdr
	}
	user := buildHeader(Request{Headers: req.Headers})
	h := hdr.Clone()
	if user.Get("Referer") == "" {
		h.Set("Referer", ref)
	}
	if p, err := url.Parse(ref); err == nil && user.Get("Origin") == "" {
		h.Set("Origin", p.Scheme+"://"+p.Host)
	}
	return h
}

// looksLikeMedia reports whether a URL names a playlist or media file, so a
// web page in its place is an error rather than something to search.
func looksLikeMedia(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	ext := strings.ToLower(path.Ext(p.Path))
	return ext == ".m3u8" || ext == ".m3u" || media.IsMediaExt(ext)
}

func (m *Manager) sendNotify(j Job) {
	name := filepath.Base(j.Output)
	if name == "." || name == "" {
		name = cmp.Or(j.Found, j.URL, j.PageURL)
	}
	var title, msg string
	switch j.State {
	case Done:
		title = "trawl: downloaded"
		msg = fmt.Sprintf("%s (%s)", name, humanSize(j.Size))
		if j.Media != nil && j.Media.Height > 0 {
			msg += fmt.Sprintf(", %dp, %s", j.Media.Height, media.Clock(j.Media.Duration))
		}
	case Duplicate:
		title, msg = "trawl: duplicate", fmt.Sprintf("already have %s (open trawl to keep a copy anyway)", filepath.Base(j.DupOf))
	default:
		title, msg = "trawl: failed", fmt.Sprintf("%s: %s", cmp.Or(j.URL, j.PageURL), j.Error)
	}
	if err := notify.Send(m.ctx, m.cfg.NotifyURL, title, msg); err != nil {
		m.log.Warn("notify", "err", err)
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
