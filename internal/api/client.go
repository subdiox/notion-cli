// Package api is a client for Notion's internal /api/v3 endpoints, the same
// API the web and desktop apps use. It authenticates with the token_v2 cookie.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/subdiox/notion-cli/internal/auth"
)

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

type Client struct {
	Session *auth.Session
	UserID  string // active user (x-notion-active-user-header)
	SpaceID string // active space (x-notion-space-id)
	HTTP    *http.Client
	Debug   bool
	// throttles (one per endpoint) are shared by copies of the client so a
	// 429 seen by one worker slows all of them down — but only for that
	// endpoint, since Notion limits endpoints independently.
	throttles *throttleSet
}

type throttleSet struct {
	mu sync.Mutex
	m  map[string]*throttle
}

func (c *Client) throttle(endpoint string) *throttle {
	if c.throttles == nil {
		c.throttles = &throttleSet{}
	}
	ts := c.throttles
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.m == nil {
		ts.m = map[string]*throttle{}
	}
	t := ts.m[endpoint]
	if t == nil {
		t = &throttle{}
		ts.m[endpoint] = t
	}
	return t
}

// Degraded reports whether an endpoint is currently being rate limited.
func (c *Client) Degraded(endpoint string) bool {
	t := c.throttle(endpoint)
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Now().Before(t.until) || (t.rate != 0 && t.rate < initialRate/2)
}

type noRetryKey struct{}

// WithoutRateLimitRetry makes 429 responses fail fast (for callers with a fallback).
func WithoutRateLimitRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRetryKey{}, true)
}

// IsRateLimited reports whether err is a 429 from Notion.
func IsRateLimited(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == 429
}

// throttle is an adaptive (AIMD) client-side rate limiter: requests are
// spaced 1/rate apart; a 429 halves the rate and pauses everyone, and each
// success raises the rate slightly. This keeps bulk reads just under Notion's
// per-user limit instead of repeatedly tripping it.
type throttle struct {
	mu    sync.Mutex
	rate  float64 // requests per second
	next  time.Time
	until time.Time // cooldown after a 429
}

const (
	minRate     = 1.0
	maxRate     = 40.0
	initialRate = 12.0
)

func (t *throttle) acquire(ctx context.Context) error {
	t.mu.Lock()
	if t.rate == 0 {
		t.rate = initialRate
	}
	now := time.Now()
	at := now
	if t.until.After(at) {
		at = t.until
	}
	if t.next.After(at) {
		at = t.next
	}
	t.next = at.Add(time.Duration(float64(time.Second) / t.rate))
	t.mu.Unlock()
	if d := time.Until(at); d > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return nil
}

func (t *throttle) success() {
	t.mu.Lock()
	t.rate = min(maxRate, t.rate+0.05)
	t.mu.Unlock()
}

func (t *throttle) limited(d time.Duration) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rate = max(minRate, t.rate/2)
	if u := time.Now().Add(d); u.After(t.until) {
		t.until = u
	}
	return t.rate
}

func New(s *auth.Session, userID, spaceID string) *Client {
	return &Client{Session: s, UserID: userID, SpaceID: spaceID, HTTP: &http.Client{Timeout: 120 * time.Second}, throttles: &throttleSet{}}
}

// APIError is a non-2xx response from Notion.
type APIError struct {
	Status   int
	Endpoint string
	Name     string `json:"name"`
	Message  string `json:"message"`
	Body     string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = strings.TrimSpace(e.Body)
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
	}
	return fmt.Sprintf("notion %s: HTTP %d %s: %s", e.Endpoint, e.Status, e.Name, msg)
}

func (c *Client) cookieHeader() string {
	parts := []string{"token_v2=" + c.Session.TokenV2}
	if c.UserID != "" {
		parts = append(parts, "notion_user_id="+c.UserID)
	}
	if len(c.Session.UserIDs) > 0 {
		b, _ := json.Marshal(c.Session.UserIDs)
		parts = append(parts, "notion_users="+url.QueryEscape(string(b)))
	}
	if c.Session.FileToken != "" {
		parts = append(parts, "file_token="+c.Session.FileToken)
	}
	if c.Session.BrowserID != "" {
		parts = append(parts, "notion_browser_id="+c.Session.BrowserID)
	}
	return strings.Join(parts, "; ")
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Cookie", c.cookieHeader())
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", c.Session.Origin)
	req.Header.Set("Referer", c.Session.Origin+"/")
	if c.UserID != "" {
		req.Header.Set("x-notion-active-user-header", c.UserID)
	}
	if c.SpaceID != "" {
		req.Header.Set("x-notion-space-id", c.SpaceID)
	}
}

// Raw POSTs body to /api/v3/<endpoint> and returns the raw response bytes.
func (c *Client) Raw(ctx context.Context, endpoint string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	u := c.Session.Origin + "/api/v3/" + strings.TrimPrefix(endpoint, "/")
	th := c.throttle(endpoint)
	noRetry, _ := ctx.Value(noRetryKey{}).(bool)
	for attempt := 0; ; attempt++ {
		if err := th.acquire(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		c.setHeaders(req)
		if c.Debug {
			fmt.Fprintf(os.Stderr, "→ POST %s %s\n", endpoint, truncate(string(payload), 2000))
		}
		start := time.Now()
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if attempt < 3 {
				time.Sleep(backoff(attempt, ""))
				continue
			}
			return nil, err
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if c.Debug {
			fmt.Fprintf(os.Stderr, "← %d %s (%s) %s\n", resp.StatusCode, endpoint, time.Since(start).Round(time.Millisecond), truncate(string(data), 2000))
		}
		if resp.StatusCode == 429 {
			d := max(backoff(attempt, resp.Header.Get("Retry-After")), 2*time.Second)
			r := th.limited(d)
			if c.Debug {
				fmt.Fprintf(os.Stderr, "rate limited on %s; pausing %s, rate now %.1f req/s\n", endpoint, d, r)
			}
			if attempt < 12 && !noRetry {
				continue
			}
		} else if resp.StatusCode/100 == 2 {
			th.success()
		}
		if resp.StatusCode >= 500 && attempt < 5 {
			time.Sleep(backoff(attempt, resp.Header.Get("Retry-After")))
			continue
		}
		if resp.StatusCode/100 != 2 {
			apiErr := &APIError{Status: resp.StatusCode, Endpoint: endpoint, Body: string(data)}
			_ = json.Unmarshal(data, apiErr)
			return nil, apiErr
		}
		return data, nil
	}
}

// Call POSTs body and decodes the JSON response into out (if non-nil).
func (c *Client) Call(ctx context.Context, endpoint string, body, out any) error {
	data, err := c.Raw(ctx, endpoint, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s response: %w", endpoint, err)
	}
	return nil
}

func backoff(attempt int, retryAfter string) time.Duration {
	if s, err := strconv.Atoi(retryAfter); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	return min(time.Duration(500*(1<<attempt))*time.Millisecond, 30*time.Second)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Download GETs a URL with the session cookies (needed for file.notion.so / signed URLs).
func (c *Client) Download(ctx context.Context, rawURL string, w io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(rawURL)
	if u != nil && (strings.HasSuffix(u.Host, "notion.so") || strings.HasSuffix(u.Host, "notion.com") || strings.HasSuffix(u.Host, "notion-static.com") || strings.HasSuffix(u.Host, "notionusercontent.com")) {
		c.setHeaders(req)
	} else {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return "", fmt.Errorf("download %s: HTTP %d: %s", rawURL, resp.StatusCode, b)
	}
	_, err = io.Copy(w, resp.Body)
	return resp.Header.Get("Content-Type"), err
}
