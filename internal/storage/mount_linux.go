//go:build linux

package storage

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func isMountedPath(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absPath = filepath.Clean(absPath)
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	best := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPoint := decodeMountInfoField(fields[4])
		mountPoint = filepath.Clean(mountPoint)
		if absPath != mountPoint && !strings.HasPrefix(absPath, mountPoint+string(os.PathSeparator)) {
			continue
		}
		if len(mountPoint) > len(best) {
			best = mountPoint
		}
	}
	return best != "" && best != string(os.PathSeparator)
}

func decodeMountInfoField(value string) string {
	var decoded strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			if code, err := strconv.ParseInt(value[i+1:i+4], 8, 32); err == nil {
				decoded.WriteByte(byte(code))
				i += 3
				continue
			}
		}
		decoded.WriteByte(value[i])
	}
	return decoded.String()
}
