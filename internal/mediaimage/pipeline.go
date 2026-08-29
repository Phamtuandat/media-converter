package mediaimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/processor"
)

type Pipeline struct {
	Magick              processor.ToolRunner
	MaxWidth, MaxHeight int
	MaxPixels           int64
	MaxOutputBytes      int64
}

func (p Pipeline) Process(ctx context.Context, input, output string, policy domain.Policy) (domain.Operation, domain.MediaDetected, error) {
	format, err := p.identify(ctx, input)
	if err != nil {
		return "", domain.MediaDetected{}, domain.NewError("image_decode_failed", "could not decode image", "image", false, err)
	}
	if format.Animated {
		return "", format, domain.NewError("unsupported_animation", "animated image is not supported", "image", false, nil)
	}
	if format.Width <= 0 || format.Height <= 0 {
		return "", format, domain.NewError("image_dimensions_unreadable", "image dimensions are invalid", "image", false, nil)
	}
	if format.Width > p.MaxWidth || format.Height > p.MaxHeight || int64(format.Width)*int64(format.Height) > p.MaxPixels {
		return "", format, domain.NewError("output_size_exceeded", "image dimensions exceed configured limits", "image", false, nil)
	}
	background := policy.AlphaBackground
	if background == "" {
		background = "#FFFFFF"
	}
	args := []string{input, "-auto-orient", "-colorspace", "sRGB", "-background", background, "-alpha", "remove", "-alpha", "off"}
	if policy.StripMetadata {
		args = append(args, "-strip")
	}
	args = append(args, "-quality", "90", "jpg:"+output)
	var toolOutput []byte
	if p.MaxOutputBytes > 0 {
		toolOutput, err = p.Magick.RunWithOutputLimit(ctx, output, p.MaxOutputBytes, args...)
	} else {
		toolOutput, err = p.Magick.Run(ctx, args...)
	}
	_ = toolOutput
	if err != nil {
		if errors.Is(err, processor.ErrOutputLimitExceeded) {
			return "", format, domain.NewError("output_size_exceeded", "image output exceeds configured size limit", "image", false, err)
		}
		return "", format, domain.NewError("converter_failed", "ImageMagick failed to encode JPEG", "image", false, err)
	}
	if _, err := p.Validate(ctx, output); err != nil {
		return "", format, err
	}
	return domain.OperationNormalized, format, nil
}

func (p Pipeline) identify(ctx context.Context, path string) (domain.MediaDetected, error) {
	out, err := p.Magick.Run(ctx, "identify", "-format", "%m|%w|%h|%n|%[colorspace]", path)
	if err != nil {
		return domain.MediaDetected{}, err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) < 5 {
		return domain.MediaDetected{}, fmt.Errorf("invalid identify output")
	}
	width, err1 := strconv.Atoi(parts[1])
	height, err2 := strconv.Atoi(parts[2])
	frames, err3 := strconv.Atoi(parts[3])
	if err1 != nil || err2 != nil {
		return domain.MediaDetected{}, fmt.Errorf("invalid image dimensions")
	}
	if err3 != nil {
		frames = 1
	}
	format := strings.ToLower(parts[0])
	mime := "image/" + format
	if format == "jpg" {
		format, mime = "jpeg", "image/jpeg"
	}
	return domain.MediaDetected{Kind: "image", Format: format, MIME: mime, Width: width, Height: height, Animated: frames > 1}, nil
}

func (p Pipeline) Validate(ctx context.Context, path string) (domain.OutputMetadata, error) {
	if _, err := os.Stat(path); err != nil {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "JPEG output is missing", "validate", false, err)
	}
	out, err := p.Magick.Run(ctx, "identify", "-format", "%m|%w|%h|%[colorspace]|%[channels]", path)
	if err != nil {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "JPEG output cannot be decoded", "validate", false, err)
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) < 5 || strings.ToUpper(parts[0]) != "JPEG" {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "output is not JPEG", "validate", false, nil)
	}
	width, widthErr := strconv.Atoi(parts[1])
	height, heightErr := strconv.Atoi(parts[2])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 || !strings.EqualFold(parts[3], "sRGB") || strings.Contains(strings.ToLower(parts[4]), "a") {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "output JPEG metadata is not canonical", "validate", false, nil)
	}
	info, err := os.Stat(path)
	if err != nil {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "JPEG output is missing", "validate", false, err)
	}
	return domain.OutputMetadata{Extension: ".jpg", MIME: "image/jpeg", Size: info.Size(), Width: width, Height: height, ColorSpace: parts[3]}, nil
}
