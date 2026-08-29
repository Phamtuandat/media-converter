//go:build darwin

package storage

import "testing"

func TestIsMountedPathRejectsLocalFilesystem(t *testing.T) {
	if isMountedPath(t.TempDir()) {
		t.Fatal("local APFS directory must not be treated as a WebDAV mount")
	}
}

func TestIsRemoteFilesystem(t *testing.T) {
	tests := map[string]bool{
		"webdav":  true,
		"fuse":    true,
		"macfuse": true,
		"apfs":    false,
		"hfs":     false,
		"":        false,
	}
	for name, want := range tests {
		if got := isRemoteFilesystem(name); got != want {
			t.Errorf("isRemoteFilesystem(%q) = %v, want %v", name, got, want)
		}
	}
}
