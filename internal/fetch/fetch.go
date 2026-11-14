// Package fetch is the shared HTTP client: one connection pool, a global
// connection limit, retries and stall detection.
package fetch

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// ErrStalled is returned when a response body delivers no data for too long.
var ErrStalled = errors.New("download stalled")

// StatusError is a non-2xx HTTP response.
type StatusError struct{ Code int }

func (e *StatusError) Error() string {
	switch e.Code {
	case 401, 403, 410:
		return fmt.Sprintf("HTTP %d (link expired or missing headers?)", e.Code)
	case 404:
		return "HTTP 404 (not found)"
	}
	return fmt.Sprintf("HTTP %d", e.Code)
}

type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// Permanent marks err as not worth retrying.
func Permanent(err error) error { return permanent{err} }

// Client wraps http.Client with a global connection limit and retries.
type Client struct {
	HTTP         *http.Client
	UserAgent    string
	Retries      int
	StallTimeout time.Duration
	conns        chan struct{}
}

// New creates a client allowing at most maxConns concurrent requests.
// HTTP/2 is off by default: many TCP connections fill a fast line better
// than streams multiplexed over one connection.
func New(maxConns, retries int, ua string, http2 bool) *Client {
	t := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     http2,
		MaxIdleConns:          maxConns * 2,
		MaxIdleConnsPerHost:   maxConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if !http2 {
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &Client{
		HTTP:         &http.Client{Transport: t},
		UserAgent:    ua,
		Retries:      retries,
		StallTimeout: 30 * time.Second,
		conns:        make(chan struct{}, maxConns),
	}
}

// Acquire takes one slot of the global connection limit.
func (c *Client) Acquire(ctx context.Context) error {
	select {
	case c.conns <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns a slot taken by Acquire.
func (c *Client) Release() { <-c.conns }

// Body is a response body that fails with ErrStalled when no data arrives
// within the client's StallTimeout.
type Body struct {
	rc      io.ReadCloser
	timer   *time.Timer
	cancel  context.CancelFunc
	stalled atomic.Bool
	d       time.Duration
}

func (b *Body) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.timer.Reset(b.d)
	}
	if err != nil && err != io.EOF && b.stalled.Load() {
		err = ErrStalled
	}
	return n, err
}

// Close stops the stall timer and closes the underlying body.
func (b *Body) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel()
	return err
}

// Get sends a GET request. rng, if set, is sent as the Range header.
// Non-2xx responses are returned as *StatusError.
func (c *Client) Get(ctx context.Context, rawURL string, h http.Header, rng string) (*http.Response, *Body, error) {
	rctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, nil, Permanent(err)
	}
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		cancel()
		return nil, nil, &StatusError{Code: resp.StatusCode}
	}
	b := &Body{rc: resp.Body, cancel: cancel, d: c.StallTimeout}
	b.timer = time.AfterFunc(c.StallTimeout, func() {
		b.stalled.Store(true)
		cancel()
	})
	return resp, b, nil
}

// Retry runs fn until it succeeds, the context ends, the error is permanent,
// or the retry budget is spent. Client errors (4xx) are retried once.
func (c *Client) Retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			wait := min(time.Duration(attempt)*time.Second, 10*time.Second)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err = fn(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var p permanent
		if errors.As(err, &p) {
			return p.error
		}
		var se *StatusError
		if errors.As(err, &se) && se.Code < 500 && se.Code != 408 && se.Code != 429 && attempt >= 1 {
			return err
		}
	}
	return err
}

// Fetch downloads a small resource (playlist, key, web page) with retries.
func (c *Client) Fetch(ctx context.Context, rawURL string, h http.Header, limit int64) ([]byte, *url.URL, error) {
	var data []byte
	var final *url.URL
	err := c.Retry(ctx, func() error {
		var err error
		data, final, err = c.FetchOnce(ctx, rawURL, h, limit)
		return err
	})
	return data, final, err
}

// FetchOnce is Fetch without retries.
func (c *Client) FetchOnce(ctx context.Context, rawURL string, h http.Header, limit int64) ([]byte, *url.URL, error) {
	if err := c.Acquire(ctx); err != nil {
		return nil, nil, err
	}
	defer c.Release()
	resp, body, err := c.Get(ctx, rawURL, h, "")
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, Permanent(fmt.Errorf("response larger than %d bytes", limit))
	}
	return data, resp.Request.URL, nil
}

// LooksLikeErrorPage reports whether data is text (HTML, XML or JSON) rather
// than media. CDNs often answer expired links with 200 and an error page.
func LooksLikeErrorPage(data []byte) bool {
	d := bytes.TrimLeft(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), " \t\r\n")
	if len(d) == 0 {
		return false
	}
	// Binary data that merely starts with '{' or '<' contains control bytes.
	for _, c := range d[:min(len(d), 512)] {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
	}
	if d[0] == '{' {
		return true
	}
	if d[0] != '<' {
		return false
	}
	head := bytes.ToLower(d[:min(len(d), 16)])
	for _, p := range []string{"<!doctype", "<html", "<head", "<body", "<?xml", "<error"} {
		if bytes.HasPrefix(head, []byte(p)) {
			return true
		}
	}
	return false
}
