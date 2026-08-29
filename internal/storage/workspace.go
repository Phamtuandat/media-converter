package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Workspace struct{ Path string }

func NewWorkspace(root, jobID, itemID string) (Workspace, error) {
	if !safePart(jobID) || !safePart(itemID) {
		return Workspace{}, fmt.Errorf("invalid workspace identity")
	}
	path := filepath.Join(root, jobID, itemID)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return Workspace{}, err
	}
	return Workspace{Path: path}, nil
}

func (w Workspace) Close() error { return os.RemoveAll(w.Path) }

func CleanupOld(root string, age time.Duration) error {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now()
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > age {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func safePart(s string) bool {
	return s != "" && s != "." && s != ".." && filepath.Base(s) == s && !strings.ContainsAny(s, `/\\`)
}
