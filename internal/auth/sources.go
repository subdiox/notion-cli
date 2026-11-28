package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Source describes where a Notion session can be imported from.
type Source struct {
	Name     string
	Aliases  []string
	Desc     string
	discover func() ([]string, error) // cookie DB paths
	read     func(path string) ([]Cookie, error)
}

func home() string { h, _ := os.UserHomeDir(); return h }

func chromiumSource(name, desc, userDataDir, keychainService string) Source {
	return Source{
		Name: name,
		Desc: desc,
		discover: func() ([]string, error) {
			base := filepath.Join(home(), "Library", "Application Support", userDataDir)
			var paths []string
			for _, pattern := range []string{"*/Cookies", "*/Network/Cookies"} {
				m, _ := filepath.Glob(filepath.Join(base, pattern))
				paths = append(paths, m...)
			}
			return paths, nil
		},
		read: func(path string) ([]Cookie, error) { return readChromiumCookies(path, keychainService) },
	}
}

// Sources lists every supported import source (macOS paths).
var Sources = []Source{
	{
		Name:    "desktop",
		Aliases: []string{"app", "notion"},
		Desc:    "Notion desktop app",
		discover: func() ([]string, error) {
			base := filepath.Join(home(), "Library", "Application Support", "Notion")
			var paths []string
			for _, p := range []string{filepath.Join(base, "Partitions", "notion", "Cookies"), filepath.Join(base, "Cookies")} {
				if _, err := os.Stat(p); err == nil {
					paths = append(paths, p)
				}
			}
			return paths, nil
		},
		read: func(path string) ([]Cookie, error) { return readChromiumCookies(path, "Notion Safe Storage") },
	},
	chromiumSource("chrome", "Google Chrome", "Google/Chrome", "Chrome Safe Storage"),
	chromiumSource("arc", "Arc", "Arc/User Data", "Arc Safe Storage"),
	chromiumSource("brave", "Brave", "BraveSoftware/Brave-Browser", "Brave Safe Storage"),
	chromiumSource("edge", "Microsoft Edge", "Microsoft Edge", "Microsoft Edge Safe Storage"),
	chromiumSource("vivaldi", "Vivaldi", "Vivaldi", "Vivaldi Safe Storage"),
	chromiumSource("chromium", "Chromium", "Chromium", "Chromium Safe Storage"),
	{
		Name: "firefox",
		Desc: "Firefox",
		discover: func() ([]string, error) {
			return filepath.Glob(filepath.Join(home(), "Library", "Application Support", "Firefox", "Profiles", "*", "cookies.sqlite"))
		},
		read: readFirefoxCookies,
	},
}

// FindSource returns the source with the given name.
func FindSource(name string) (Source, error) {
	for _, s := range Sources {
		if s.Name == name || slices.Contains(s.Aliases, name) {
			return s, nil
		}
	}
	names := make([]string, len(Sources))
	for i, s := range Sources {
		names[i] = s.Name
	}
	return Source{}, fmt.Errorf("unknown source %q (available: %s)", name, strings.Join(names, ", "))
}

func readFirefoxCookies(path string) ([]Cookie, error) {
	tmp, err := os.MkdirTemp("", "notion-cli-ff-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dst := filepath.Join(tmp, "cookies.sqlite")
	for _, suffix := range []string{"", "-wal"} {
		if b, err := os.ReadFile(path + suffix); err == nil {
			_ = os.WriteFile(dst+suffix, b, 0o600)
		}
	}
	db, err := sql.Open("sqlite", "file:"+dst+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT host, name, value FROM moz_cookies WHERE host LIKE '%notion.so' OR host LIKE '%notion.com'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cookie
	for rows.Next() {
		var c Cookie
		if err := rows.Scan(&c.Host, &c.Name, &c.Value); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Session is everything needed to talk to Notion's internal API.
type Session struct {
	Source    string   `json:"source"`
	Origin    string   `json:"origin"` // e.g. https://app.notion.com or https://www.notion.so
	TokenV2   string   `json:"token_v2"`
	FileToken string   `json:"file_token,omitempty"`
	UserIDs   []string `json:"user_ids"`
	BrowserID string   `json:"browser_id,omitempty"`
	DeviceID  string   `json:"device_id,omitempty"`
}

// Candidate is a session found in one cookie store (browser profile).
type Candidate struct {
	Session *Session
	Path    string // cookie DB the session came from
}

// Profile returns a short human-readable label for the cookie store, e.g.
// "Profile 1" for Chrome or the profile directory name for Firefox.
func (c Candidate) Profile() string {
	dir := filepath.Dir(c.Path)
	if filepath.Base(dir) == "Network" {
		dir = filepath.Dir(dir)
	}
	return filepath.Base(dir)
}

// Candidates returns every Notion session found in the named source, one per
// cookie store (browser profile) that holds a token_v2. "auto" (or "")
// searches all sources. Sessions with the same token_v2 are reported once.
// The error is non-nil only when nothing was found; it then explains why
// each source failed.
func Candidates(sourceName string) ([]Candidate, error) {
	var srcs []Source
	if sourceName == "auto" || sourceName == "" {
		srcs = Sources
	} else {
		src, err := FindSource(sourceName)
		if err != nil {
			return nil, err
		}
		srcs = []Source{src}
	}
	var out []Candidate
	var errs []string
	seen := map[string]bool{}
	for _, src := range srcs {
		paths, err := src.discover()
		if err != nil {
			errs = append(errs, src.Name+": "+err.Error())
			continue
		}
		if len(paths) == 0 {
			errs = append(errs, src.Name+": no cookie store found")
			continue
		}
		found := false
		for _, p := range paths {
			cookies, err := src.read(p)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s (%s): %v", src.Name, p, err))
				continue
			}
			s := sessionFromCookies(cookies)
			if s == nil {
				continue
			}
			found = true
			if seen[s.TokenV2] {
				continue
			}
			seen[s.TokenV2] = true
			s.Source = src.Name
			out = append(out, Candidate{Session: s, Path: p})
		}
		if !found {
			errs = append(errs, src.Name+": no Notion login (token_v2) in "+strconv.Itoa(len(paths))+" cookie store(s)")
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no Notion login found:\n  %s", strings.Join(errs, "\n  "))
	}
	return out, nil
}

func sessionFromCookies(cookies []Cookie) *Session {
	// Prefer cookies on the most specific/current domain: app.notion.com > notion.com > notion.so.
	rank := func(host string) int {
		h := strings.TrimPrefix(host, ".")
		switch {
		case h == "app.notion.com":
			return 0
		case strings.HasSuffix(h, "notion.com"):
			return 1
		case h == "www.notion.so":
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(cookies, func(i, j int) bool { return rank(cookies[i].Host) < rank(cookies[j].Host) })
	get := func(name string) (string, string) {
		for _, c := range cookies {
			if c.Name == name && c.Value != "" {
				return c.Value, c.Host
			}
		}
		return "", ""
	}
	token, host := get("token_v2")
	if token == "" {
		return nil
	}
	s := &Session{TokenV2: token}
	if strings.HasSuffix(host, "notion.com") {
		s.Origin = "https://app.notion.com"
	} else {
		s.Origin = "https://www.notion.so"
	}
	s.FileToken, _ = get("file_token")
	s.BrowserID, _ = get("notion_browser_id")
	s.DeviceID, _ = get("device_id")
	if users, _ := get("notion_users"); users != "" {
		if dec, err := url.QueryUnescape(users); err == nil {
			users = dec
		}
		_ = json.Unmarshal([]byte(users), &s.UserIDs)
	}
	if len(s.UserIDs) == 0 {
		if uid, _ := get("notion_user_id"); uid != "" {
			s.UserIDs = []string{uid}
		}
	}
	return s
}

var ErrNotLoggedIn = errors.New("not logged in: run `notion auth import` first")
