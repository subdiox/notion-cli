// Package api is a client for Notion's internal /api/v3 endpoints, the same
// API the web and desktop apps use. It authenticates with the token_v2 cookie.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
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
}

func New(s *auth.Session, userID, spaceID string) *Client {
	return &Client{Session: s, UserID: userID, SpaceID: spaceID, HTTP: &http.Client{Timeout: 120 * time.Second}}
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
	for attempt := 0; ; attempt++ {
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
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			if attempt < 5 {
				time.Sleep(backoff(attempt, resp.Header.Get("Retry-After")))
				continue
			}
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
	return time.Duration(500*(1<<attempt)) * time.Millisecond
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
