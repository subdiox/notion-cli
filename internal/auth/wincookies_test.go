package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsWSLRelease(t *testing.T) {
	for in, want := range map[string]bool{
		"5.15.167.4-microsoft-standard-WSL2": true,
		"4.4.0-19041-Microsoft":              true,
		"6.8.0-45-generic":                   false,
		"":                                   false,
	} {
		if got := isWSLRelease(in); got != want {
			t.Errorf("isWSLRelease(%q) = %v, want %v", in, got, want)
		}
	}
}

// encryptGCM builds a Windows Chromium "v10" value.
func encryptGCM(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	return append(append([]byte("v10"), nonce...), gcm.Seal(nil, nonce, plain, nil)...)
}

func TestDecryptGCM(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	enc := encryptGCM(t, key, []byte("secret-value"))
	got, err := decryptGCM(enc, key)
	if err != nil || string(got) != "secret-value" {
		t.Fatalf("decryptGCM = %q, %v", got, err)
	}
	wrong := make([]byte, 32)
	if _, err := decryptGCM(enc, wrong); err == nil {
		t.Fatal("decryptGCM with a wrong key succeeded")
	}
	if _, err := decryptGCM([]byte("v20xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), key); err == nil {
		t.Fatal("decryptGCM accepted a v20 value")
	}
}

func TestWindowsDecrypterRejectsAppBound(t *testing.T) {
	_, err := windowsDecrypter("/nonexistent/Cookies")([]byte("v20whatever"))
	if err == nil || !strings.Contains(err.Error(), "app-bound") {
		t.Fatalf("err = %v, want app-bound encryption error", err)
	}
}

// fakeDPAPI makes "DPAPI" blobs reversible in tests: protect = prefix "blob:".
func fakeDPAPI(t *testing.T) {
	t.Helper()
	orig := dpapiUnprotect
	dpapiUnprotect = func(blob []byte) ([]byte, error) {
		return []byte(strings.TrimPrefix(string(blob), "blob:")), nil
	}
	t.Cleanup(func() { dpapiUnprotect = orig })
}

func writeLocalState(t *testing.T, dir string, key []byte) {
	t.Helper()
	enc := base64.StdEncoding.EncodeToString(append([]byte("DPAPIblob:"), key...))
	b, _ := json.Marshal(map[string]any{"os_crypt": map[string]any{"encrypted_key": enc}})
	if err := os.WriteFile(filepath.Join(dir, "Local State"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCookieKey(t *testing.T) {
	fakeDPAPI(t)
	dir := t.TempDir()
	key := []byte(strings.Repeat("k", 32))
	writeLocalState(t, dir, key)
	got, err := windowsCookieKey(filepath.Join(dir, "Local State"))
	if err != nil || string(got) != string(key) {
		t.Fatalf("windowsCookieKey = %q, %v", got, err)
	}
}

func TestFindLocalState(t *testing.T) {
	root := t.TempDir()
	// Desktop app layout: Notion/Local State, Notion/Partitions/notion/Network/Cookies.
	db := filepath.Join(root, "Notion", "Partitions", "notion", "Network", "Cookies")
	os.MkdirAll(filepath.Dir(db), 0o700)
	os.WriteFile(filepath.Join(root, "Notion", "Local State"), []byte("{}"), 0o600)
	got, err := findLocalState(db)
	if err != nil || got != filepath.Join(root, "Notion", "Local State") {
		t.Fatalf("findLocalState = %q, %v", got, err)
	}
}

// TestReadWindowsChromiumCookies decrypts a Chromium cookie DB laid out like
// the Windows Notion desktop app's, with meta version 24 (SHA256(host) prefix).
func TestReadWindowsChromiumCookies(t *testing.T) {
	fakeDPAPI(t)
	root := t.TempDir()
	base := filepath.Join(root, "Notion")
	dbPath := filepath.Join(base, "Partitions", "notion", "Network", "Cookies")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("x", 32))
	writeLocalState(t, base, key)

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE meta (key TEXT, value TEXT)`,
		`INSERT INTO meta VALUES ('version', '24')`,
		`CREATE TABLE cookies (host_key TEXT, name TEXT, value TEXT, encrypted_value BLOB)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	host := ".app.notion.com"
	h := sha256.Sum256([]byte(host))
	enc := encryptGCM(t, key, append(h[:], "tok"...))
	if _, err := db.Exec(`INSERT INTO cookies VALUES (?, 'token_v2', '', ?), ('.app.notion.com', 'notion_user_id', 'u1', x'')`, host, enc); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cookies, err := readChromiumCookies(dbPath, windowsDecrypter(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	s := sessionFromCookies(cookies)
	if s == nil || s.TokenV2 != "tok" || s.Origin != "https://app.notion.com" || len(s.UserIDs) != 1 || s.UserIDs[0] != "u1" {
		t.Fatalf("session = %+v", s)
	}
}
