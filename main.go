// trawl is a small self-hosted downloader for HLS (.m3u8) streams and plain
// files, built to fill a fast line with parallel connections.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/503stack/trawl/internal/config"
	"github.com/503stack/trawl/internal/fetch"
	"github.com/503stack/trawl/internal/queue"
	"github.com/503stack/trawl/internal/web"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			log.Warn(bin + " not found in PATH: HLS remuxing and media checks will fail")
		}
	}
	for _, dir := range []string{cfg.DataDir, cfg.TempDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	for _, f := range cfg.Folders {
		if err := os.MkdirAll(f.Path, 0o755); err != nil {
			return err
		}
	}

	client := fetch.New(cfg.MaxConnections, cfg.Retries, cfg.UserAgent, cfg.HTTP2, cfg.IP)
	m, err := queue.New(cfg, client, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m.Start(ctx)

	srv := &http.Server{Addr: cfg.Listen, Handler: web.Handler(m, cfg), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("trawl starting", "version", version, "listen", cfg.Listen, "folders", len(cfg.Folders),
		"max_jobs", cfg.MaxJobs, "max_connections", cfg.MaxConnections, "job_connections", cfg.JobConnections)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	m.Wait()
	return nil
}
