package mediavideo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/processor"
)

func TestProcessLeavesOutputValidationToManager(t *testing.T) {
	ffprobe := writeVideoExecutable(t, "#!/bin/sh\nexit 1\n")
	root := t.TempDir()
	input := filepath.Join(root, "input.mp4")
	output := filepath.Join(root, "output.mp4")
	if err := os.WriteFile(input, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline := Pipeline{
		Probe: mediavideoProbe(ffprobe), MaxWidth: 100, MaxHeight: 100,
		MaxDurationMS: 1000, MaxOutputBytes: 1024,
	}
	detected := domain.MediaDetected{
		Kind: "video", Container: "mp4", Width: 10, Height: 10, DurationMS: 100,
		VideoCodec: "h264", PixelFormat: "yuv420p", Faststart: true,
	}
	if _, _, err := pipeline.Process(context.Background(), input, output, detected); err != nil {
		t.Fatalf("process should not perform a second validation: %v", err)
	}
	if _, err := pipeline.Validate(context.Background(), output); err == nil {
		t.Fatal("manager validation should reject the invalid output")
	}
}

func mediavideoProbe(path string) FFProbe {
	return FFProbe{Runner: processor.ToolRunner{Path: path}}
}

func writeVideoExecutable(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool.sh")
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
