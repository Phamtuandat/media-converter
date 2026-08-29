//go:build darwin

package storage

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
)

// macOS does not expose Linux's /proc/self/mountinfo. Comparing the statfs
// identity of the nearest existing path with its parent still lets us detect
// a mounted WebDAV/FUSE filesystem without creating a local fallback tree.
func isMountedPath(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absPath = filepath.Clean(absPath)
	_, mountedFS, ok := existingFS(absPath)
	if !ok {
		return false
	}
	mountPath := statfsString(mountedFS.Mntonname[:])
	if mountPath == "" || mountPath == string(os.PathSeparator) {
		return false
	}
	// A path below the writable APFS data volume is also different from its
	// parent volume on macOS. Only treat remote WebDAV/FUSE filesystems as the
	// required output mount; otherwise an unmounted target becomes a false
	// positive local directory.
	if !isRemoteFilesystem(statfsString(mountedFS.Fstypename[:])) {
		return false
	}
	parent := filepath.Dir(mountPath)
	_, parentFS, ok := existingFS(parent)
	return ok && (!reflect.DeepEqual(mountedFS.Fsid, parentFS.Fsid) || mountedFS.Fstypename != parentFS.Fstypename)
}

func isRemoteFilesystem(name string) bool {
	switch name {
	case "webdav", "fuse", "fusefs", "fuseblk", "osxfuse", "macfuse":
		return true
	default:
		return false
	}
}

func statfsString(value []int8) string {
	end := 0
	for end < len(value) && value[end] != 0 {
		end++
	}
	return string(bytesFromInt8(value[:end]))
}

func bytesFromInt8(value []int8) []byte {
	result := make([]byte, len(value))
	for i, item := range value {
		result[i] = byte(item)
	}
	return result
}

func existingFS(path string) (string, syscall.Statfs_t, bool) {
	path = filepath.Clean(path)
	for {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err == nil {
			return path, stat, true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", syscall.Statfs_t{}, false
		}
		path = parent
	}
}
