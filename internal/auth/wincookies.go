package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Windows support. The Notion desktop app and the browsers keep their cookies
// in the Windows user's profile, and Chromium protects the cookie key in
// "Local State" with DPAPI, which only Windows can unprotect for that user.
// Both a native Windows build (notion.exe from cmd / PowerShell) and a Linux
// build running under WSL read them: the WSL build reaches the profile through
// the drvfs mount (/mnt/c/...) and asks powershell.exe for DPAPI
// (wincookies_other.go), the Windows build calls the APIs directly
// (wincookies_windows.go).

// isWSL reports whether a Linux build runs under WSL (1 or 2).
var isWSL = sync.OnceValue(func() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return isWSLRelease(string(b))
})

func isWSLRelease(osrelease string) bool {
	return strings.Contains(strings.ToLower(osrelease), "microsoft")
}

// onWindows reports whether the import sources are Windows apps: a native
// Windows build, or a Linux build under WSL.
func onWindows() bool { return runtime.GOOS == "windows" || isWSL() }

// windowsDirs holds the Windows user's folders, as paths this process can open.
type windowsDirs struct {
	LocalAppData string // %LOCALAPPDATA% (…\AppData\Local)
	AppData      string // %APPDATA% (…\AppData\Roaming)
}

// getWindowsDirs is a variable so that tests can replace it.
var getWindowsDirs = sync.OnceValues(platformWindowsDirs)

// dpapiUnprotect decrypts a DPAPI blob of the current Windows user. A variable
// so that tests can replace it.
var dpapiUnprotect = platformDPAPIUnprotect

// findLocalState returns the "Local State" file of the Chromium user data
// directory that contains cookieDB (…/User Data/<profile>/Network/Cookies, or
// …/Notion/Partitions/notion/Network/Cookies for the desktop app).
func findLocalState(cookieDB string) (string, error) {
	dir := filepath.Dir(cookieDB)
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "Local State")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no \"Local State\" file above %s", cookieDB)
}

// windowsCookieKey reads the AES-256 cookie key that Chromium on Windows keeps
// DPAPI-protected in "Local State" (os_crypt.encrypted_key, base64 of "DPAPI" + blob).
func windowsCookieKey(localStatePath string) ([]byte, error) {
	b, err := os.ReadFile(localStatePath)
	if err != nil {
		return nil, err
	}
	var ls struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(b, &ls); err != nil {
		return nil, fmt.Errorf("parse %s: %w", localStatePath, err)
	}
	if ls.OSCrypt.EncryptedKey == "" {
		return nil, fmt.Errorf("no os_crypt.encrypted_key in %s", localStatePath)
	}
	raw, err := base64.StdEncoding.DecodeString(ls.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("decode os_crypt.encrypted_key: %w", err)
	}
	if !bytes.HasPrefix(raw, []byte("DPAPI")) {
		return nil, errors.New("os_crypt.encrypted_key is not DPAPI-protected")
	}
	key, err := dpapiUnprotect(raw[len("DPAPI"):])
	if err != nil {
		return nil, fmt.Errorf("DPAPI unprotect: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("unexpected cookie key length %d", len(key))
	}
	return key, nil
}

// readError explains the usual failure on Windows: Chromium apps (the Notion
// desktop app included) keep their cookie DB open exclusively while they run.
func readError(err error) error {
	if onWindows() && isLockError(err) {
		return fmt.Errorf("cannot read the cookie store: Windows locks it while the app is running; "+
			"quit the app completely (also from the tray) and try again (%w)", err)
	}
	return err
}
