//go:build windows

package auth

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformWindowsDirs() (windowsDirs, error) {
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return windowsDirs{}, err
	}
	roaming, err := windows.KnownFolderPath(windows.FOLDERID_RoamingAppData, 0)
	if err != nil {
		return windowsDirs{}, err
	}
	return windowsDirs{LocalAppData: local, AppData: roaming}, nil
}

func platformDPAPIUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("empty DPAPI blob")
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// isLockError: a file another process opened without sharing.
func isLockError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, os.ErrPermission)
}
