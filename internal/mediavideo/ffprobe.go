package mediavideo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/processor"
)

type FFProbe struct{ Runner processor.ToolRunner }

type probeResult struct {
	Format struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		CodecType  string            `json:"codec_type"`
		CodecName  string            `json:"codec_name"`
		Profile    string            `json:"profile"`
		SampleRate string            `json:"sample_rate"`
		Channels   int               `json:"channels"`
		Width      int               `json:"width"`
		Height     int               `json:"height"`
		PixFmt     string            `json:"pix_fmt"`
		Tags       map[string]string `json:"tags"`
		SideData   []struct {
			Rotation int `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
}

func (p FFProbe) Probe(ctx context.Context, path string) (domain.MediaDetected, error) {
	if _, err := os.Stat(path); err != nil {
		return domain.MediaDetected{}, err
	}
	out, err := p.Runner.Run(ctx, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	if err != nil {
		return domain.MediaDetected{}, domain.NewError("video_probe_failed", "ffprobe could not read video", "probe", false, err)
	}
	var raw probeResult
	if err := json.Unmarshal(out, &raw); err != nil {
		return domain.MediaDetected{}, domain.NewError("video_probe_failed", "ffprobe returned invalid JSON", "probe", false, err)
	}
	if len(raw.Streams) == 0 {
		return domain.MediaDetected{}, domain.NewError("corrupt_media", "video has no streams", "probe", false, nil)
	}
	videoCount, audioCount := 0, 0
	detected := domain.MediaDetected{Container: containerFromProbe(raw), MIME: "video/mp4", StreamCount: len(raw.Streams)}
	if detected.Container == "mov" {
		detected.MIME = "video/quicktime"
	}
	for _, stream := range raw.Streams {
		switch stream.CodecType {
		case "video":
			videoCount++
			detected.VideoCodec = stream.CodecName
			detected.Width = stream.Width
			detected.Height = stream.Height
			detected.PixelFormat = stream.PixFmt
			for _, sd := range stream.SideData {
				detected.Rotation = sd.Rotation
			}
			if detected.Rotation == 0 {
				if v, ok := stream.Tags["rotate"]; ok {
					detected.Rotation, _ = strconv.Atoi(v)
				}
			}
		case "audio":
			audioCount++
			detected.AudioCodec = stream.CodecName
			detected.AudioProfile = stream.Profile
			detected.Channels = stream.Channels
			detected.SampleRate, _ = strconv.Atoi(stream.SampleRate)
		}
	}
	detected.HasAudio = audioCount > 0
	if videoCount == 0 && audioCount > 0 {
		detected.Kind = "audio"
		detected.Container, detected.MIME = audioContainerFromProbe(raw)
	} else if videoCount > 0 {
		detected.Kind = "video"
	} else {
		return domain.MediaDetected{}, domain.NewError("corrupt_media", "media stream is missing", "probe", false, nil)
	}
	if raw.Format.Duration != "" {
		seconds, _ := strconv.ParseFloat(raw.Format.Duration, 64)
		detected.DurationMS = int64(seconds * 1000)
	}
	detected.Faststart = hasFaststart(path)
	return detected, nil
}

func (p FFProbe) ImageDimensions(ctx context.Context, path string) (int, int, error) {
	out, err := p.Runner.Run(ctx, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=p=0:s=x", path)
	if err != nil {
		return 0, 0, err
	}
	var width, height int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%dx%d", &width, &height); err != nil {
		return 0, 0, err
	}
	return width, height, nil
}

func firstContainer(name string) string {
	if name == "" {
		return ""
	}
	return strings.Split(name, ",")[0]
}

func containerFromProbe(raw probeResult) string {
	brand := strings.ToLower(strings.TrimSpace(raw.Format.Tags["major_brand"]))
	if brand == "qt  " || brand == "qt" {
		return "mov"
	}
	if brand != "" {
		return "mp4"
	}
	if strings.Contains(strings.ToLower(raw.Format.FormatName), "quicktime") && !strings.Contains(strings.ToLower(raw.Format.FormatName), "mp4") {
		return "mov"
	}
	return "mp4"
}

func audioContainerFromProbe(raw probeResult) (string, string) {
	format := strings.ToLower(firstContainer(raw.Format.FormatName))
	switch format {
	case "ogg", "oga":
		return "ogg", "audio/ogg"
	case "wav", "wavpack":
		return "wav", "audio/wav"
	case "mov", "mp4", "m4a":
		return "m4a", "audio/mp4"
	default:
		return format, "audio/" + format
	}
}
func FormatError(err error) error { return fmt.Errorf("ffprobe: %w", err) }
