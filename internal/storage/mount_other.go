//go:build !linux && !darwin

package storage

// WebDAV deployments are supported on the Linux VPS target. Fail closed on
// other platforms so a missing mount cannot silently become local output.
func isMountedPath(string) bool { return false }
