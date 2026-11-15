package httpdl

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/503stack/trawl/internal/fetch"
)

// TestConnectionLimit mimics a CDN that serves at most two connections per
// client and answers the rest with 503: the download must still complete,
// settling at two connections.
func TestConnectionLimit(t *testing.T) {
	data := make([]byte, 2<<20)
	rand.Read(data)
	var inflight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-" {
			if inflight.Add(1) > 2 {
				inflight.Add(-1)
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			defer inflight.Add(-1)
			time.Sleep(5 * time.Millisecond) // keep connections overlapping
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	d := &Downloader{Client: fetch.New(32, 3, "test", false, "auto"), Workers: 8, ChunkSize: 64 << 10}
	dst := filepath.Join(t.TempDir(), "out")
	res, err := d.Download(context.Background(), srv.URL+"/f.mp4", dst, func(int64, int64) {})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	if res.Connections < 1 || res.Connections > 2 {
		t.Errorf("connections = %d, want 1 or 2", res.Connections)
	}
}
