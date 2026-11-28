package auth

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// Cookie is a decrypted browser cookie.
type Cookie struct {
	Host  string
	Name  string
	Value string
}

// keychainPassword reads the "Safe Storage" secret that Chromium-based apps
// keep in the macOS login keychain. macOS may show an access prompt.
func keychainPassword(service string) (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-w", "-s", service).Output()
	if err != nil {
		return "", fmt.Errorf("read keychain item %q: %w (allow access in the macOS prompt)", service, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// cookieDecrypter decrypts one encrypted_value into its plaintext (still with
// the SHA256(host) prefix of newer Chromium versions).
type cookieDecrypter func(enc []byte) ([]byte, error)

// macDecrypter: AES-128-CBC with a key derived from the keychain secret.
func macDecrypter(keychainService string) cookieDecrypter {
	var key []byte
	return func(enc []byte) ([]byte, error) {
		if key == nil {
			pw, err := keychainPassword(keychainService)
			if err != nil {
				return nil, err
			}
			if key, err = pbkdf2.Key(sha1.New, pw, []byte("saltysalt"), 1003, 16); err != nil {
				return nil, err
			}
		}
		return decryptCBC(enc, key)
	}
}

// windowsDecrypter: AES-256-GCM with the DPAPI-protected key from the
// "Local State" file of the user data directory that holds dbPath.
func windowsDecrypter(dbPath string) cookieDecrypter {
	var key []byte
	return func(enc []byte) ([]byte, error) {
		if len(enc) >= 3 && string(enc[:3]) == "v20" {
			return nil, errors.New("encrypted with Chrome's app-bound encryption (v20), which only the browser itself can decrypt; " +
				"use the Notion desktop app, Firefox, or `notion auth set-token`")
		}
		if key == nil {
			ls, err := findLocalState(dbPath)
			if err != nil {
				return nil, err
			}
			if key, err = windowsCookieKey(ls); err != nil {
				return nil, err
			}
		}
		return decryptGCM(enc, key)
	}
}

// copyCookieDB copies a cookie DB (and its WAL/journal) to a temp directory so
// that a running browser's lock doesn't block us. Call the returned cleanup.
func copyCookieDB(dbPath, name string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "notion-cli-cookies-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	dst := filepath.Join(tmp, name)
	for _, suffix := range []string{"", "-wal", "-journal"} {
		b, err := os.ReadFile(dbPath + suffix)
		if err != nil {
			if suffix == "" {
				cleanup()
				return "", nil, readError(err)
			}
			continue
		}
		if err := os.WriteFile(dst+suffix, b, 0o600); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return dst, cleanup, nil
}

// readChromiumCookies decrypts Notion cookies from a Chromium-format cookie DB.
func readChromiumCookies(dbPath string, decrypt cookieDecrypter) ([]Cookie, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	dst, cleanup, err := copyCookieDB(dbPath, "Cookies")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	db, err := sql.Open("sqlite", "file:"+dst+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	metaVersion := 0
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='version'`).Scan(&v); err == nil {
		metaVersion, _ = strconv.Atoi(v)
	}

	rows, err := db.Query(`SELECT host_key, name, value, encrypted_value FROM cookies
		WHERE host_key LIKE '%notion.so' OR host_key LIKE '%notion.com'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cookies []Cookie
	for rows.Next() {
		var host, name, plain string
		var enc []byte
		if err := rows.Scan(&host, &name, &plain, &enc); err != nil {
			return nil, err
		}
		val := plain
		if val == "" && len(enc) > 0 {
			out, err := decrypt(enc)
			if err != nil {
				return nil, fmt.Errorf("decrypt %s/%s: %w", host, name, err)
			}
			val = stripHostHash(out, host, metaVersion)
		}
		cookies = append(cookies, Cookie{Host: host, Name: name, Value: val})
	}
	return cookies, rows.Err()
}

// stripHostHash removes the SHA256(host_key) prefix that Chromium >= 130
// (cookie DB meta version 24) puts before the value.
func stripHostHash(out []byte, host string, metaVersion int) string {
	if metaVersion >= 24 && len(out) >= 32 {
		h := sha256.Sum256([]byte(host))
		if bytes.Equal(out[:32], h[:]) {
			out = out[32:]
		}
	}
	return string(out)
}

// decryptCBC decrypts a macOS "v10"/"v11" value (AES-128-CBC, IV of spaces).
func decryptCBC(enc, key []byte) ([]byte, error) {
	if len(enc) < 3 || (string(enc[:3]) != "v10" && string(enc[:3]) != "v11") {
		return nil, errors.New("unsupported cookie encryption prefix")
	}
	data := enc[3:]
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("invalid ciphertext length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	iv := bytes.Repeat([]byte{' '}, aes.BlockSize)
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(out) {
		return nil, errors.New("bad padding (wrong keychain key?)")
	}
	return out[:len(out)-pad], nil
}

// decryptGCM decrypts a Windows "v10" value: AES-256-GCM with a 12-byte nonce
// after the prefix and the 16-byte tag at the end.
func decryptGCM(enc, key []byte) ([]byte, error) {
	if len(enc) < 3 || string(enc[:3]) != "v10" {
		return nil, errors.New("unsupported cookie encryption prefix")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	data := enc[3:]
	if len(data) < gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("invalid ciphertext length")
	}
	out, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("%w (wrong key?)", err)
	}
	return out, nil
}
