package queue

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/503stack/trawl/internal/hls"
	"github.com/503stack/trawl/internal/httpdl"
	"github.com/503stack/trawl/internal/media"
	"github.com/503stack/trawl/internal/naming"
	"github.com/503stack/trawl/internal/notify"
)

var errDuplicate = errors.New("duplicate")

// download is what the transfer step produced.
type download struct {
	path     string
	isHLS    bool
	names    []string // filename candidates: Content-Disposition, URL path
	ctype    string
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
	if req.Name == "" && req.PageURL != "" {
		t, err := pageTitle(ctx, m, req.PageURL, hdr)
		if err != nil {
			m.note(j, "could not read page title: %v", err)
		}
		title = t
	}

	dl, err := m.transfer(ctx, j, r, work, hdr)
	if err != nil {
		return err
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
func (m *Manager) transfer(ctx context.Context, j *Job, r *run, work string, hdr http.Header) (*download, error) {
	req := j.Request
	u, _ := url.Parse(req.URL)
	p := strings.ToLower(u.Path)
	if !strings.HasSuffix(p, ".m3u8") && !strings.HasSuffix(p, ".m3u") {
		m.update(j, func(j *Job) { j.Kind = "http" })
		d := &httpdl.Downloader{Client: m.client, Header: hdr, Workers: m.cfg.JobConnections, ChunkSize: m.cfg.ChunkSize}
		dst := filepath.Join(work, "download")
		res, err := d.Download(ctx, req.URL, dst, func(done, total int64) {
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
				}, nil
			}
		}
		os.Remove(dst)
		r.bytes.Store(0)
		r.total.Store(0)
	}
	return m.transferHLS(ctx, j, r, work, hdr)
}

func (m *Manager) transferHLS(ctx context.Context, j *Job, r *run, work string, hdr http.Header) (*download, error) {
	req := j.Request
	m.update(j, func(j *Job) { j.Kind = "hls" })
	plan, err := hls.Load(ctx, m.client, hdr, req.URL, req.Upgrade == nil || *req.Upgrade)
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
	src, _ := url.Parse(plan.Source)
	return &download{
		path:     out,
		isHLS:    true,
		names:    []string{path.Base(src.Path), path.Base(strings.TrimSuffix(path.Dir(src.Path), "/"))},
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

var (
	metaTitleRe = regexp.MustCompile(`(?is)<meta\s[^>]*(?:property|name)\s*=\s*["'](?:og:title|twitter:title)["'][^>]*>`)
	contentRe   = regexp.MustCompile(`(?is)content\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	titleRe     = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// pageTitle reads og:title (or <title>) from a web page.
func pageTitle(ctx context.Context, m *Manager, pageURL string, hdr http.Header) (string, error) {
	h := hdr.Clone()
	h.Del("Origin")
	h.Del("Referer")
	data, _, err := m.client.Fetch(ctx, pageURL, h, 4<<20)
	if err != nil {
		return "", err
	}
	if tag := metaTitleRe.Find(data); tag != nil {
		if c := contentRe.FindSubmatch(tag); c != nil {
			if t := strings.TrimSpace(html.UnescapeString(string(c[1]) + string(c[2]))); t != "" {
				return t, nil
			}
		}
	}
	if t := titleRe.FindSubmatch(data); t != nil {
		return strings.TrimSpace(html.UnescapeString(string(t[1]))), nil
	}
	return "", errors.New("no title found")
}

func (m *Manager) sendNotify(j Job) {
	name := filepath.Base(j.Output)
	if name == "." || name == "" {
		name = j.URL
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
		title, msg = "trawl: failed", fmt.Sprintf("%s: %s", j.URL, j.Error)
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
