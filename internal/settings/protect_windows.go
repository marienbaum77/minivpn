package settings

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// protect encrypts for the current Windows user: another account, or the
// file copied to another PC, cannot decrypt it.
func protect(plain []byte) ([]byte, error) {
	in := blob(plain)
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return take(&out), nil
}

func unprotect(enc []byte) ([]byte, error) {
	in := blob(enc)
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	return take(&out), nil
}

func blob(b []byte) windows.DataBlob {
	if len(b) == 0 {
		return windows.DataBlob{}
	}
	return windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func take(b *windows.DataBlob) []byte {
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(b.Data)))
	return append([]byte(nil), unsafe.Slice(b.Data, b.Size)...)
}
