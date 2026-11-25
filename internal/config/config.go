package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/subdiox/notion-cli/internal/auth"
)

// Config holds user-selected defaults.
type Config struct {
	ActiveUserID  string `json:"active_user_id,omitempty"`
	ActiveSpaceID string `json:"active_space_id,omitempty"`
}

func Dir() string {
	if d := os.Getenv("NOTION_CLI_HOME"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "notion-cli")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "notion-cli")
}

func readJSON(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(name string, v any) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(Dir(), name), b, 0o600)
}

func LoadSession() (*auth.Session, error) {
	if tok := os.Getenv("NOTION_TOKEN_V2"); tok != "" {
		s := &auth.Session{Source: "env", TokenV2: tok, Origin: "https://www.notion.so"}
		if o := os.Getenv("NOTION_ORIGIN"); o != "" {
			s.Origin = o
		}
		return s, nil
	}
	var s auth.Session
	if err := readJSON("session.json", &s); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, auth.ErrNotLoggedIn
		}
		return nil, err
	}
	return &s, nil
}

func SaveSession(s *auth.Session) error { return writeJSON("session.json", s) }

func DeleteSession() error {
	err := os.Remove(filepath.Join(Dir(), "session.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func Load() (*Config, error) {
	var c Config
	if err := readJSON("config.json", &c); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &c, nil
}

func Save(c *Config) error { return writeJSON("config.json", c) }
