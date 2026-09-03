package mediavideo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/processor"
)

type Pipeline struct {
	FFmpeg              processor.ToolRunner
	Probe               FFProbe
	Magick              processor.ToolRunner
	MaxWidth, MaxHeight int
	MaxDurationMS       int64
	MaxOutputBytes      int64
	FFmpegThreads       int
}

func (p Pipeline) Process(ctx context.Context, input, output string, detected domain.MediaDetected) (domain.Operation, domain.MediaDetected, error) {
	if detected.Width > p.MaxWidth || detected.Height > p.MaxHeight || detected.DurationMS > p.MaxDurationMS {
		return "", detected, domain.NewError("output_size_exceeded", "video exceeds configured limits", "video", false, nil)
	}
	operation := classify(detected)
	if detected.Rotation != 0 {
		operation = domain.OperationTranscoded
	}
	var args []string
	switch operation {
	case domain.OperationPassthrough:
		if err := copyFileLimited(input, output, p.MaxOutputBytes); err != nil {
			if _, ok := err.(*domain.Error); ok {
				return "", detected, err
			}
			return "", detected, domain.NewError("converter_failed", "could not copy video", "video", false, err)
		}
	case domain.OperationRemuxed:
		args = []string{"-y", "-i", input, "-map", "0:v:0", "-map", "0:a?", "-c", "copy", "-movflags", "+faststart", output}
		if _, err := p.FFmpeg.RunWithOutputLimit(ctx, output, p.MaxOutputBytes, args...); err != nil {
			if errors.Is(err, processor.ErrOutputLimitExceeded) {
				return "", detected, domain.NewError("output_size_exceeded", "video output exceeds configured size limit", "video", false, err)
			}
			return "", detected, domain.NewError("converter_failed", "FFmpeg remux failed", "video", false, err)
		}
	case domain.OperationTranscoded:
		args = []string{"-y", "-i", input, "-map", "0:v:0", "-map", "0:a?", "-vf", transcodeFilter(detected), "-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "medium", "-crf", "23", "-threads", fmt.Sprint(p.FFmpegThreads), "-movflags", "+faststart", "-metadata:s:v:0", "rotate=0", output}
		if detected.HasAudio {
			args = append(args[:len(args)-1], "-c:a", "aac", "-b:a", "128k", args[len(args)-1])
		}
		if _, err := p.FFmpeg.RunWithOutputLimit(ctx, output, p.MaxOutputBytes, args...); err != nil {
			if errors.Is(err, processor.ErrOutputLimitExceeded) {
				return "", detected, domain.NewError("output_size_exceeded", "video output exceeds configured size limit", "video", false, err)
			}
			return "", detected, domain.NewError("converter_failed", "FFmpeg transcode failed", "video", false, err)
		}
	}
	return operation, detected, nil
}

func rotationFilter(rotation int) string {
	rotation %= 360
	if rotation < 0 {
		rotation += 360
	}
	switch rotation {
	case 90:
		return "transpose=1"
	case 180:
		return "hflip,vflip"
	case 270:
		return "transpose=2"
	default:
		return "null"
	}
}

func transcodeFilter(m domain.MediaDetected) string {
	filters := []string{rotationFilter(m.Rotation)}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(m.PixelFormat)), "yuvj") {
		filters = append(filters, "scale=in_range=full:out_range=tv")
	}
	filters = append(filters, "format=yuv420p")
	return strings.Join(filters, ",")
}

func classify(m domain.MediaDetected) domain.Operation {
	canonicalCodec := strings.EqualFold(m.VideoCodec, "h264") || strings.EqualFold(m.VideoCodec, "avc1")
	canonicalAudio := !m.HasAudio || strings.EqualFold(m.AudioCodec, "aac")
	if m.Container == "mp4" && m.Rotation == 0 && canonicalCodec && canonicalAudio && strings.EqualFold(m.PixelFormat, "yuv420p") && m.Faststart {
		return domain.OperationPassthrough
	}
	if m.Rotation == 0 && canonicalCodec && canonicalAudio && strings.EqualFold(m.PixelFormat, "yuv420p") {
		return domain.OperationRemuxed
	}
	return domain.OperationTranscoded
}

func (p Pipeline) Validate(ctx context.Context, path string) (domain.OutputMetadata, error) {
	m, err := p.Probe.Probe(ctx, path)
	if err != nil {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "output could not be probed", "validate", false, err)
	}
	if m.Container != "mp4" || m.StreamCount > 2 || !strings.EqualFold(m.VideoCodec, "h264") || !strings.EqualFold(m.PixelFormat, "yuv420p") || m.Rotation != 0 || (m.HasAudio && !strings.EqualFold(m.AudioCodec, "aac")) {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "output does not satisfy canonical video contract", "validate", false, nil)
	}
	if m.Width <= 0 || m.Height <= 0 || m.DurationMS < 0 {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "output dimensions or duration are invalid", "validate", false, nil)
	}
	if !hasFaststart(path) {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "output is missing faststart", "validate", false, nil)
	}
	info, err := os.Stat(path)
	if err != nil {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "output is missing", "validate", false, err)
	}
	return domain.OutputMetadata{Extension: ".mp4", MIME: "video/mp4", Size: info.Size(), Width: m.Width, Height: m.Height, DurationMS: m.DurationMS, VideoCodec: "h264", AudioCodec: m.AudioCodec, PixelFormat: "yuv420p", Faststart: true}, nil
}

func (p Pipeline) Thumbnail(ctx context.Context, input, output string) (domain.OutputMetadata, error) {
	filter := `scale=w=min(640\,iw):h=min(360\,ih):force_original_aspect_ratio=decrease`
	args := []string{"-y", "-ss", "1", "-i", input, "-frames:v", "1", "-vf", filter, "-q:v", "3", "-map_metadata", "-1", output}
	if _, err := p.FFmpeg.RunWithOutputLimit(ctx, output, p.MaxOutputBytes, args...); err != nil {
		args = []string{"-y", "-i", input, "-frames:v", "1", "-vf", filter, "-q:v", "3", "-map_metadata", "-1", output}
		if _, err := p.FFmpeg.RunWithOutputLimit(ctx, output, p.MaxOutputBytes, args...); err != nil {
			return domain.OutputMetadata{}, err
		}
	}
	if filepath.Ext(output) != ".jpg" {
		return domain.OutputMetadata{}, fmt.Errorf("thumbnail must be jpg")
	}
	info, err := os.Stat(output)
	if err != nil {
		return domain.OutputMetadata{}, err
	}
	width, height, err := p.Probe.ImageDimensions(ctx, output)
	if err != nil || width <= 0 || height <= 0 || width > 640 || height > 360 {
		return domain.OutputMetadata{}, fmt.Errorf("thumbnail dimensions are invalid")
	}
	if p.Magick.Path != "" {
		imageInfo, identifyErr := p.Magick.Run(ctx, "identify", "-format", "%m", output)
		if identifyErr != nil || strings.TrimSpace(string(imageInfo)) != "JPEG" {
			return domain.OutputMetadata{}, fmt.Errorf("thumbnail is not a readable JPEG")
		}
	}
	return domain.OutputMetadata{Extension: ".jpg", MIME: "image/jpeg", Size: info.Size(), Width: width, Height: height}, nil
}

func copyFileLimited(src, dst string, limit int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if err == nil && limit > 0 && n > limit {
		return domain.NewError("output_size_exceeded", "video output exceeds configured size limit", "video", false, nil)
	}
	return err
}
