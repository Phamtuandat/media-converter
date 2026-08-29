package detection

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/processor"
)

type Detector struct{ Magick processor.ToolRunner }

func (d Detector) Detect(ctx context.Context, path string, maxBytes int64) (domain.MediaDetected, error) {
	info, err := os.Stat(path)
	if err != nil {
		return domain.MediaDetected{}, domain.NewError("storage_read_failed", "could not read input", "detect", true, err)
	}
	if info.Size() > maxBytes {
		return domain.MediaDetected{}, domain.NewError("output_size_exceeded", "input exceeds configured size limit", "detect", false, nil)
	}
	f, err := os.Open(path)
	if err != nil {
		return domain.MediaDetected{}, domain.NewError("storage_read_failed", "could not open input", "detect", true, err)
	}
	defer f.Close()
	header := make([]byte, 64)
	n, _ := io.ReadFull(f, header)
	header = header[:n]
	if len(header) >= 3 && string(header[:3]) == "\xff\xd8\xff" {
		return d.detectImage(ctx, path, "jpeg", "image/jpeg", false)
	}
	if len(header) >= 8 && string(header[:8]) == "\x89PNG\r\n\x1a\n" {
		return d.detectImage(ctx, path, "png", "image/png", false)
	}
	if len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP" {
		return d.detectImage(ctx, path, "webp", "image/webp", false)
	}
	if len(header) >= 12 && string(header[4:8]) == "ftyp" {
		brand := string(header[8:12])
		if isImageBrand(brand) {
			return d.detectImage(ctx, path, strings.ToLower(brand), "image/"+strings.ToLower(brand), false)
		}
		container := "mp4"
		mime := "video/mp4"
		if brand == "qt  " {
			container, mime = "mov", "video/quicktime"
		}
		return domain.MediaDetected{Kind: "video", Container: container, MIME: mime}, nil
	}
	return domain.MediaDetected{}, domain.NewError("unsupported_format", "input signature is not supported", "detect", false, nil)
}

func (d Detector) detectImage(ctx context.Context, path, format, mime string, animated bool) (domain.MediaDetected, error) {
	if d.Magick.Path == "" {
		return domain.MediaDetected{}, domain.NewError("converter_unavailable", "ImageMagick is unavailable", "detect", false, nil)
	}
	out, err := d.Magick.Run(ctx, "identify", "-format", "%w|%h|%n|%[colorspace]", path)
	if err != nil {
		return domain.MediaDetected{}, domain.NewError("image_decode_failed", "image parser could not read input", "detect", false, err)
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) < 4 {
		return domain.MediaDetected{}, domain.NewError("image_dimensions_unreadable", "image dimensions could not be read", "detect", false, nil)
	}
	var width, height, frames int
	if _, err := fmt.Sscanf(parts[0], "%d", &width); err != nil {
		return domain.MediaDetected{}, domain.NewError("image_dimensions_unreadable", "image width could not be read", "detect", false, err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &height); err != nil {
		return domain.MediaDetected{}, domain.NewError("image_dimensions_unreadable", "image height could not be read", "detect", false, err)
	}
	if _, err := fmt.Sscanf(parts[2], "%d", &frames); err != nil {
		frames = 1
	}
	return domain.MediaDetected{Kind: "image", Format: format, MIME: mime, Animated: animated || frames > 1, Width: width, Height: height}, nil
}

func isImageBrand(brand string) bool {
	return brand == "avif" || brand == "avis" || brand == "mif1" || brand == "msf1" || brand == "heic" || brand == "heix"
}

func readUint32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
