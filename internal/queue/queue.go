// Package queue holds the job list, runs downloads on a fixed number of
// workers and persists jobs to disk.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/503stack/trawl/internal/config"
	"github.com/503stack/trawl/internal/fetch"
	"github.com/503stack/trawl/internal/media"
	"github.com/503stack/trawl/internal/naming"
)

// State is a job's lifecycle state.
type State string

const (
	Queued    State = "queued"
	Running   State = "running"
	Verifying State = "verifying"
	Done      State = "done"
	Failed    State = "failed"
	Canceled  State = "canceled"
	Duplicate State = "duplicate"
)

func (s State) active() bool { return s == Running || s == Verifying }

// Request is what the user submits.
type Request struct {
	URL     string `json:"url"`
	PageURL string `json:"page_url,omitempty"`
	Name    string `json:"name,omitempty"`
	Folder  string `json:"folder,omitempty"`
	Headers string `json:"headers,omitempty"` // "Name: value" per line
	Verify  string `json:"verify,omitempty"`  // overrides TRAWL_VERIFY
	Upgrade *bool  `json:"upgrade,omitempty"` // look for better HLS quality, default true
	Dedupe  string `json:"dedupe,omitempty"`  // overrides TRAWL_DEDUPE: skip | keep | off
	// Candidates are video and playlist URLs the bookmarklet saw the browser
	// load. With URL empty, trawl picks the best of these and the page's own.
	Candidates []string `json:"candidates,omitempty"`
}

// Job is a request plus its progress and result.
type Job struct {
	ID string `json:"id"`
	Request
	State    State       `json:"state"`
	Kind     string      `json:"kind,omitempty"`      // hls | http
	Found    string      `json:"found_url,omitempty"` // picked from the page when URL was empty
	Error    string      `json:"error,omitempty"`
	Notes    []string    `json:"notes,omitempty"`
	Created  time.Time   `json:"created"`
	Started  time.Time   `json:"started,omitzero"`
	Finished time.Time   `json:"finished,omitzero"`
	Bytes    int64       `json:"bytes"`
	Total    int64       `json:"total"` // -1 or 0: unknown
	Segments int         `json:"segments,omitempty"`
	SegDone  int         `json:"seg_done,omitempty"`
	Speed    float64     `json:"speed,omitempty"` // bytes/s
	Output   string      `json:"output,omitempty"`
	Failed   string      `json:"failed_path,omitempty"`
	DupOf    string      `json:"duplicate_of,omitempty"` // existing identical file
	Held     string      `json:"held,omitempty"`         // duplicate kept aside until Keep or removal
	HeldName string      `json:"held_name,omitempty"`    // name it would have been saved as
	Size     int64       `json:"size,omitempty"`
	SHA256   string      `json:"sha256,omitempty"`
	Media    *media.Info `json:"media,omitempty"`
}

type run struct {
	cancel    context.CancelFunc
	bytes     atomic.Int64
	total     atomic.Int64
	segDone   atomic.Int64
	segTotal  atomic.Int64
	lastBytes int64
	speed     float64
}

// Manager owns all jobs.
type Manager struct {
	cfg    *config.Config
	client *fetch.Client
	namer  *naming.Namer
	log    *slog.Logger
	file   string

	mu   sync.Mutex
	jobs []*Job
	runs map[string]*run
	wake chan struct{}
	wg   sync.WaitGroup
	ctx  context.Context
}

// New loads saved jobs. Jobs that were running when trawl stopped are queued
// again and start from scratch.
func New(cfg *config.Config, client *fetch.Client, log *slog.Logger) (*Manager, error) {
	m := &Manager{
		cfg:    cfg,
		client: client,
		namer:  &naming.Namer{Sep: cfg.NameSeparator, ASCII: cfg.NameASCII, MaxBytes: cfg.NameMaxBytes, Strip: cfg.NameStrip},
		log:    log,
		file:   filepath.Join(cfg.DataDir, "jobs.json"),
		runs:   map[string]*run{},
		wake:   make(chan struct{}),
	}
	data, err := os.ReadFile(m.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m.jobs); err != nil {
			return nil, fmt.Errorf("%s: %w", m.file, err)
		}
	}
	for _, j := range m.jobs {
		if j.State.active() {
			j.State = Queued
			j.Notes = append(j.Notes, "restarted after trawl was stopped")
		}
	}
	return m, nil
}

// Start launches the workers. They stop when ctx ends; Wait waits for them.
func (m *Manager) Start(ctx context.Context) {
	m.ctx = ctx
	for range m.cfg.MaxJobs {
		m.wg.Add(1)
		go m.worker(ctx)
	}
	go m.speedTicker(ctx)
}

// Wait blocks until all workers have stopped and state is saved.
func (m *Manager) Wait() {
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveLocked()
}

// Add validates and queues a request.
func (m *Manager) Add(req Request) (Job, error) {
	if req.URL == "" && req.PageURL == "" {
		return Job{}, errors.New("url or page_url is required")
	}
	if req.URL != "" && !isHTTP(req.URL) {
		return Job{}, errors.New("url must be an http(s) URL")
	}
	if req.PageURL != "" && !isHTTP(req.PageURL) {
		return Job{}, errors.New("page_url must be an http(s) URL")
	}
	if len(req.Candidates) > 50 {
		req.Candidates = req.Candidates[:50]
	}
	if _, ok := m.cfg.Folder(req.Folder); !ok {
		return Job{}, fmt.Errorf("unknown folder %q", req.Folder)
	}
	if req.Verify != "" && !config.ValidVerify(req.Verify) {
		return Job{}, errors.New("verify must be transfer, probe or decode")
	}
	if req.Dedupe != "" && req.Dedupe != "skip" && req.Dedupe != "keep" && req.Dedupe != "off" {
		return Job{}, errors.New("dedupe must be skip, keep or off")
	}
	id := make([]byte, 6)
	rand.Read(id)
	j := &Job{ID: hex.EncodeToString(id), Request: req, State: Queued, Created: time.Now()}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs = append(m.jobs, j)
	m.saveLocked()
	m.wakeLocked()
	return *j, nil
}

func isHTTP(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// List returns a snapshot of all jobs, newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for i := len(m.jobs) - 1; i >= 0; i-- {
		j := *m.jobs[i]
		j.Notes = slices.Clone(j.Notes)
		if r := m.runs[j.ID]; r != nil {
			j.Bytes, j.Total = r.bytes.Load(), r.total.Load()
			j.SegDone, j.Segments = int(r.segDone.Load()), int(r.segTotal.Load())
			j.Speed = r.speed
		}
		out = append(out, j)
	}
	return out
}

// Cancel stops a queued or running job.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.find(id)
	switch {
	case j == nil:
		return errNotFound
	case j.State == Queued:
		j.State, j.Finished = Canceled, time.Now()
		m.saveLocked()
	case j.State.active():
		m.runs[id].cancel()
	default:
		return errors.New("job is not queued or running")
	}
	return nil
}

// Retry queues a finished job again, optionally with a fresh URL.
func (m *Manager) Retry(id, newURL string) error {
	if newURL != "" {
		if !isHTTP(newURL) {
			return errors.New("url must be an http(s) URL")
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.find(id)
	if j == nil {
		return errNotFound
	}
	if j.State == Queued || j.State.active() {
		return errors.New("job is already queued or running")
	}
	if newURL != "" {
		j.URL = newURL
	}
	dropHeld(j)
	*j = Job{ID: j.ID, Request: j.Request, State: Queued, Created: j.Created}
	m.saveLocked()
	m.wakeLocked()
	return nil
}

// Remove deletes a job that is not running from the list (not its file).
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, j := range m.jobs {
		if j.ID == id {
			if j.State.active() {
				return errors.New("cancel the job first")
			}
			dropHeld(j)
			m.jobs = slices.Delete(m.jobs, i, i+1)
			m.saveLocked()
			return nil
		}
	}
	return errNotFound
}

// ClearFinished removes all done, duplicate and canceled jobs.
func (m *Manager) ClearFinished() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs = slices.DeleteFunc(m.jobs, func(j *Job) bool {
		if j.State == Done || j.State == Duplicate || j.State == Canceled {
			dropHeld(j)
			return true
		}
		return false
	})
	m.saveLocked()
}

// Keep saves a held duplicate into its folder after all. name (optional)
// replaces the suggested name; a taken name still gets a _1 suffix.
func (m *Manager) Keep(id, name string) error {
	m.mu.Lock()
	j := m.find(id)
	if j == nil {
		m.mu.Unlock()
		return errNotFound
	}
	if j.State != Duplicate || j.Held == "" {
		m.mu.Unlock()
		return errors.New("job has no held duplicate to keep")
	}
	held := j.Held
	base, ext := naming.SplitExt(j.HeldName)
	if name != "" {
		if b, e := naming.SplitExt(name); e == ext {
			name = b // "clip.mp4" → "clip"; the extension stays what the file is
		}
		if base = m.namer.Clean(name); base == "" {
			m.mu.Unlock()
			return errors.New("name is empty after cleaning")
		}
	}
	folder, _ := m.cfg.Folder(j.Folder)
	j.Held = "" // claim it so a second click cannot move it twice
	m.mu.Unlock()

	final, err := naming.Place(held, folder.Path, base, ext, m.cfg.NameSeparator)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		j.Held = held
		return err
	}
	j.State, j.Output, j.HeldName = Done, final, ""
	j.Notes = append(j.Notes, "kept although identical to "+filepath.Base(j.DupOf))
	m.saveLocked()
	return nil
}

// dropHeld deletes a held duplicate file.
func dropHeld(j *Job) {
	if j.Held != "" {
		os.Remove(j.Held)
		j.Held, j.HeldName = "", ""
	}
}

var errNotFound = errors.New("job not found")

// IsNotFound reports whether err means the job does not exist.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }

func (m *Manager) find(id string) *Job {
	for _, j := range m.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (m *Manager) wakeLocked() {
	close(m.wake)
	m.wake = make(chan struct{})
}

func (m *Manager) saveLocked() {
	data, err := json.MarshalIndent(m.jobs, "", "  ")
	if err == nil {
		tmp := m.file + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, m.file)
		}
	}
	if err != nil {
		m.log.Error("saving jobs", "err", err)
	}
}

// update changes a job under the lock.
func (m *Manager) update(j *Job, fn func(*Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(j)
}

func (m *Manager) note(j *Job, format string, args ...any) {
	m.update(j, func(j *Job) { j.Notes = append(j.Notes, fmt.Sprintf(format, args...)) })
}

func (m *Manager) worker(ctx context.Context) {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		var j *Job
		for _, c := range m.jobs {
			if c.State == Queued {
				j = c
				break
			}
		}
		wake := m.wake
		var r *run
		var rctx context.Context
		if j != nil && ctx.Err() == nil {
			r = &run{}
			rctx, r.cancel = context.WithCancel(ctx)
			m.runs[j.ID] = r
			j.State, j.Started = Running, time.Now()
			m.saveLocked()
		}
		m.mu.Unlock()
		if r == nil {
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		err := m.pipeline(rctx, j, r)
		r.cancel()
		m.finish(ctx, j, r, err)
	}
}

func (m *Manager) finish(ctx context.Context, j *Job, r *run, err error) {
	m.mu.Lock()
	j.Bytes, j.Total = r.bytes.Load(), r.total.Load()
	j.SegDone, j.Segments = int(r.segDone.Load()), int(r.segTotal.Load())
	j.Finished = time.Now()
	delete(m.runs, j.ID)
	switch {
	case ctx.Err() != nil:
		j.State = Queued
		j.Notes = append(j.Notes, "interrupted by shutdown, will restart")
	case errors.Is(err, errDuplicate):
		j.State = Duplicate
	case errors.Is(err, context.Canceled):
		j.State = Canceled
	case err != nil:
		j.State, j.Error = Failed, err.Error()
	default:
		j.State = Done
	}
	m.saveLocked()
	snap := *j
	m.mu.Unlock()

	m.log.Info("job finished", "id", snap.ID, "state", snap.State, "output", snap.Output, "err", snap.Error)
	if m.cfg.NotifyURL != "" && (snap.State == Done || snap.State == Failed || snap.State == Duplicate) {
		go m.sendNotify(snap)
	}
}

func (m *Manager) speedTicker(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.mu.Lock()
		for _, r := range m.runs {
			b := r.bytes.Load()
			r.speed = 0.6*float64(b-r.lastBytes) + 0.4*r.speed
			r.lastBytes = b
		}
		m.mu.Unlock()
	}
}
