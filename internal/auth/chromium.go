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

// readChromiumCookies decrypts Notion cookies from a Chromium-format cookie DB.
func readChromiumCookies(dbPath, keychainService string) ([]Cookie, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	// Copy the DB (and WAL/journal) so a running browser's lock doesn't block us.
	tmp, err := os.MkdirTemp("", "notion-cli-cookies-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dst := filepath.Join(tmp, "Cookies")
	for _, suffix := range []string{"", "-wal", "-journal"} {
		b, err := os.ReadFile(dbPath + suffix)
		if err != nil {
			continue
		}
		if err := os.WriteFile(dst+suffix, b, 0o600); err != nil {
			return nil, err
		}
	}

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

	var key []byte
	var cookies []Cookie
	for rows.Next() {
		var host, name, plain string
		var enc []byte
		if err := rows.Scan(&host, &name, &plain, &enc); err != nil {
			return nil, err
		}
		val := plain
		if val == "" && len(enc) > 0 {
			if key == nil {
				pw, err := keychainPassword(keychainService)
				if err != nil {
					return nil, err
				}
				key, err = pbkdf2.Key(sha1.New, pw, []byte("saltysalt"), 1003, 16)
				if err != nil {
					return nil, err
				}
			}
			dec, err := decryptChromium(enc, key, host, metaVersion)
			if err != nil {
				return nil, fmt.Errorf("decrypt %s/%s: %w", host, name, err)
			}
			val = dec
		}
		cookies = append(cookies, Cookie{Host: host, Name: name, Value: val})
	}
	return cookies, rows.Err()
}

func decryptChromium(enc, key []byte, host string, metaVersion int) (string, error) {
	if len(enc) < 3 || (string(enc[:3]) != "v10" && string(enc[:3]) != "v11") {
		return "", errors.New("unsupported cookie encryption prefix")
	}
	data := enc[3:]
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return "", errors.New("invalid ciphertext length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	iv := bytes.Repeat([]byte{' '}, aes.BlockSize)
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(out) {
		return "", errors.New("bad padding (wrong keychain key?)")
	}
	out = out[:len(out)-pad]
	// Chromium >= 130 (cookie DB meta version 24) prefixes SHA256(host_key).
	if metaVersion >= 24 && len(out) >= 32 {
		h := sha256.Sum256([]byte(host))
		if bytes.Equal(out[:32], h[:]) {
			out = out[32:]
		}
	}
	return string(out), nil
}
