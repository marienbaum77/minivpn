//go:build !windows

package settings

// The app runs on Windows only; elsewhere (CI, tests) the key is stored as is.
func protect(plain []byte) ([]byte, error) { return plain, nil }

func unprotect(enc []byte) ([]byte, error) { return enc, nil }
