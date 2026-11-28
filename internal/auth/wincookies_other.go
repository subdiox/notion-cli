//go:build !windows

package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Under WSL the Windows folders and DPAPI are reached through powershell.exe
// (WSL interop) and wslpath.

func platformWindowsDirs() (windowsDirs, error) {
	if !isWSL() {
		return windowsDirs{}, errors.New("not running on Windows or WSL")
	}
	out, err := runPowerShell(`[Environment]::GetFolderPath('LocalApplicationData'); [Environment]::GetFolderPath('ApplicationData')`, "")
	if err != nil {
		return windowsDirs{}, err
	}
	lines := nonEmptyLines(out)
	if len(lines) != 2 {
		return windowsDirs{}, fmt.Errorf("unexpected output from powershell.exe: %q", out)
	}
	var d windowsDirs
	for i, p := range []*string{&d.LocalAppData, &d.AppData} {
		u, err := exec.Command("wslpath", "-u", lines[i]).Output()
		if err != nil {
			return windowsDirs{}, fmt.Errorf("wslpath %q: %w", lines[i], err)
		}
		*p = strings.TrimSpace(string(u))
	}
	return d, nil
}

func platformDPAPIUnprotect(blob []byte) ([]byte, error) {
	out, err := runPowerShell(`Add-Type -AssemblyName System.Security; `+
		`$b = [Convert]::FromBase64String([Console]::In.ReadToEnd().Trim()); `+
		`[Convert]::ToBase64String([Security.Cryptography.ProtectedData]::Unprotect($b, $null, 'CurrentUser'))`,
		base64.StdEncoding.EncodeToString(blob))
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(out))
}

// isLockError: drvfs reports a file another Windows process holds
// exclusively as EACCES.
func isLockError(err error) bool { return errors.Is(err, os.ErrPermission) }

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// powerShellPath finds powershell.exe even when WSL does not append the
// Windows PATH (appendWindowsPath=false).
func powerShellPath() (string, error) {
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p, nil
	}
	p := "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", errors.New("powershell.exe not found (is WSL interop enabled?)")
}

// runPowerShell runs a script with Windows PowerShell and returns its stdout
// (UTF-8). stdin is passed to the script as [Console]::In.
func runPowerShell(script, stdin string) (string, error) {
	ps, err := powerShellPath()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command",
		"[Console]::OutputEncoding = [Text.Encoding]::UTF8; $ErrorActionPreference = 'Stop'; "+script)
	// A Windows program started from a Linux directory warns about UNC paths.
	if _, err := os.Stat("/mnt/c"); err == nil {
		cmd.Dir = "/mnt/c"
	}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("powershell.exe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
