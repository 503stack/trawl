package queue

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/503stack/trawl/internal/config"
	"github.com/503stack/trawl/internal/fetch"
)

// makeHLS renders a 6 second AES-128 encrypted HLS stream with ffmpeg.
func makeHLS(t *testing.T, dir string) {
	t.Helper()
	key := make([]byte, 16)
	rand.Read(key)
	keyFile := filepath.Join(t.TempDir(), "enc.key")
	os.WriteFile(keyFile, key, 0o644)
	os.WriteFile(filepath.Join(dir, "enc.key"), key, 0o644)
	info := filepath.Join(t.TempDir(), "keyinfo")
	os.WriteFile(info, []byte("enc.key\n"+keyFile+"\n"), 0o644)
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440", "-t", "6", "-c:v", "libx264", "-g", "25", "-c:a", "aac",
		"-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod", "-hls_key_info_file", info,
		"-hls_segment_filename", filepath.Join(dir, "seg%03d.ts"), filepath.Join(dir, "720p.m3u8"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	// 360p.m3u8 is the "bad" link the user pastes; trawl should find 720p.m3u8.
	b, _ := os.ReadFile(filepath.Join(dir, "720p.m3u8"))
	os.WriteFile(filepath.Join(dir, "360p.m3u8"), b, 0o644)
}

type env struct {
	m      *Manager
	cfg    *config.Config
	srv    *httptest.Server
	cancel context.CancelFunc
}

func setup(t *testing.T, mux *http.ServeMux) *env {
	t.Helper()
	cfg := config.Defaults()
	root := t.TempDir()
	cfg.DataDir = filepath.Join(root, "data")
	cfg.TempDir = filepath.Join(root, "data", "tmp")
	cfg.Folders = []config.Folder{{Name: "videos", Path: filepath.Join(root, "videos")}}
	cfg.ChunkSize = 256 << 10
	cfg.JobConnections = 4
	cfg.MinSize = 1000
	cfg.NameStrip = regexp.MustCompile(`\s*[-–]\s*TestSite$`)
	os.MkdirAll(cfg.TempDir, 0o755)
	client := fetch.New(cfg.MaxConnections, 2, cfg.UserAgent, false)
	m, err := New(cfg, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	e := &env{m: m, cfg: cfg, srv: httptest.NewServer(mux), cancel: cancel}
	t.Cleanup(func() { cancel(); m.Wait(); e.srv.Close() })
	return e
}

func (e *env) run(t *testing.T, req Request) Job {
	t.Helper()
	j, err := e.m.Add(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, x := range e.m.List() {
			if x.ID == j.ID && x.State != Queued && !x.State.active() {
				return x
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", j.ID)
	return Job{}
}

func TestHLS(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	hlsDir := t.TempDir()
	makeHLS(t, hlsDir)
	mux := http.NewServeMux()
	files := http.FileServer(http.Dir(hlsDir))
	var referer string
	mux.Handle("/hls/", http.StripPrefix("/hls/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != referer {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		files.ServeHTTP(w, r)
	})))
	mux.HandleFunc("/watch", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><head><meta property="og:title" content="Sunset Över Öresund – TestSite"></head></html>`)
	})
	e := setup(t, mux)
	referer = e.srv.URL + "/watch"
	req := Request{URL: e.srv.URL + "/hls/360p.m3u8", PageURL: referer}

	j := e.run(t, req)
	if j.State != Done {
		t.Fatalf("state %s: %s (notes %v)", j.State, j.Error, j.Notes)
	}
	if filepath.Base(j.Output) != "Sunset_Over_Oresund.mp4" {
		t.Errorf("output = %s", j.Output)
	}
	if j.Media == nil || j.Media.Height != 240 || j.Media.ACodec != "aac" || j.Media.Duration < 5.5 || j.Media.Duration > 6.5 {
		t.Errorf("media = %+v", j.Media)
	}
	if !strings.Contains(strings.Join(j.Notes, ";"), "upgraded 360p → 720p") {
		t.Errorf("notes = %v", j.Notes)
	}
	if j.Segments != 6 || j.SegDone != 6 {
		t.Errorf("segments %d/%d", j.SegDone, j.Segments)
	}

	// Same stream again: identical file → duplicate, nothing written.
	if j2 := e.run(t, req); j2.State != Duplicate || j2.Output != j.Output {
		t.Errorf("second run: %s %s", j2.State, j2.Output)
	}

	// Without the Referer the CDN says 403.
	j3 := e.run(t, Request{URL: e.srv.URL + "/hls/720p.m3u8"})
	if j3.State != Failed || !strings.Contains(j3.Error, "403") {
		t.Errorf("no referer: %s %q", j3.State, j3.Error)
	}
}

func TestHTTPRanges(t *testing.T) {
	a := make([]byte, 3<<20+123)
	b := make([]byte, 2<<20)
	rand.Read(a)
	rand.Read(b)
	serve := func(data []byte, ranges bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !ranges {
				r.Header.Del("Range")
			}
			http.ServeContent(w, r, "data.bin", time.Time{}, bytes.NewReader(data))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/a/data.bin", serve(a, true))
	mux.HandleFunc("/b/data.bin", serve(b, false))
	mux.HandleFunc("/expired/clip.mp4", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<!DOCTYPE html><html><body>Link expired</body></html>")
	})
	e := setup(t, mux)

	j := e.run(t, Request{URL: e.srv.URL + "/a/data.bin"})
	if j.State != Done || filepath.Base(j.Output) != "data.bin" {
		t.Fatalf("a: %s %s %s", j.State, j.Output, j.Error)
	}
	if got, _ := os.ReadFile(j.Output); !bytes.Equal(got, a) {
		t.Error("a: content differs")
	}
	// Different content, same name, server without range support → data_1.bin.
	j = e.run(t, Request{URL: e.srv.URL + "/b/data.bin"})
	if j.State != Done || filepath.Base(j.Output) != "data_1.bin" {
		t.Fatalf("b: %s %s %s", j.State, j.Output, j.Error)
	}
	if got, _ := os.ReadFile(j.Output); !bytes.Equal(got, b) {
		t.Error("b: content differs")
	}

	j = e.run(t, Request{URL: e.srv.URL + "/expired/clip.mp4"})
	if j.State != Failed || !strings.Contains(j.Error, "web page") || j.Failed == "" {
		t.Errorf("expired: %s %q failed_path=%q", j.State, j.Error, j.Failed)
	}
}
