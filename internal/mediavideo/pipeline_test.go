package mediavideo

import (
	"testing"

	"media-converter-v2/internal/domain"
)

func TestClassifyVideo(t *testing.T) {
	tests := []struct {
		name  string
		media domain.MediaDetected
		want  domain.Operation
	}{
		{"passthrough", domain.MediaDetected{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", HasAudio: true, PixelFormat: "yuv420p", Faststart: true}, domain.OperationPassthrough},
		{"remux", domain.MediaDetected{Container: "mov", VideoCodec: "h264", AudioCodec: "aac", HasAudio: true, PixelFormat: "yuv420p"}, domain.OperationRemuxed},
		{"transcode hevc", domain.MediaDetected{Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac", HasAudio: true, PixelFormat: "yuv420p"}, domain.OperationTranscoded},
		{"transcode pixel format", domain.MediaDetected{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", HasAudio: true, PixelFormat: "yuv444p"}, domain.OperationTranscoded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classify(test.media); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestRotationFilter(t *testing.T) {
	tests := map[int]string{90: "transpose=1", 180: "hflip,vflip", 270: "transpose=2", 0: "null", -90: "transpose=2"}
	for rotation, want := range tests {
		if got := rotationFilter(rotation); got != want {
			t.Fatalf("rotation %d: got %q, want %q", rotation, got, want)
		}
	}
	if got := classify(domain.MediaDetected{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", HasAudio: true, PixelFormat: "yuv420p", Faststart: true, Rotation: 90}); got != domain.OperationTranscoded {
		t.Fatalf("rotated video classified as %q", got)
	}
}

func TestTranscodeFilterNormalizesPixelFormat(t *testing.T) {
	tests := []struct {
		name  string
		media domain.MediaDetected
		want  string
	}{
		{
			name:  "limited range",
			media: domain.MediaDetected{PixelFormat: "yuv420p"},
			want:  "null,format=yuv420p",
		},
		{
			name:  "full range yuvj",
			media: domain.MediaDetected{PixelFormat: "yuvj420p"},
			want:  "null,scale=in_range=full:out_range=tv,format=yuv420p",
		},
		{
			name:  "full range with rotation",
			media: domain.MediaDetected{PixelFormat: "YUVJ420P", Rotation: 90},
			want:  "transpose=1,scale=in_range=full:out_range=tv,format=yuv420p",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := transcodeFilter(test.media); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}
