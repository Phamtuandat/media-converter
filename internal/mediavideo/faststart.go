package mediavideo

import (
	"encoding/binary"
	"io"
	"os"
)

func hasFaststart(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	var offset int64
	for offset+8 <= info.Size() {
		var header [8]byte
		if _, err := io.ReadFull(f, header[:]); err != nil {
			return false
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		typ := string(header[4:8])
		headerSize := int64(8)
		if size == 1 {
			var ext [8]byte
			if _, err := io.ReadFull(f, ext[:]); err != nil {
				return false
			}
			size = int64(binary.BigEndian.Uint64(ext[:]))
			headerSize = 16
		}
		if size == 0 {
			size = info.Size() - offset
		}
		if size < headerSize || size > info.Size()-offset {
			return false
		}
		if typ == "moov" {
			return true
		}
		if typ == "mdat" {
			return false
		}
		if _, err := f.Seek(size-headerSize, io.SeekCurrent); err != nil {
			return false
		}
		offset += size
	}
	return false
}
