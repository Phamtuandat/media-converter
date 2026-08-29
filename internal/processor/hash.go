package processor

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"syscall"
)

func HashFile(path string) (string, int64, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", 0, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		return "", 0, os.ErrInvalid
	}
	defer f.Close()
	return HashReader(f)
}

func HashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
