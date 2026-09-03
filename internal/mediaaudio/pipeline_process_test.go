package mediaaudio

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/mediavideo"
	"media-converter-v2/internal/processor"
)

func TestProcessLeavesOutputValidationToManager(t *testing.T) {
	ffmpeg := writeAudioExecutable(t, `#!/bin/sh
last=""
for arg do last="$arg"; done
: > "$last"
`)
	ffprobe := writeAudioExecutable(t, "#!/bin/sh\nexit 1\n")
	root := t.TempDir()
	input := filepath.Join(root, "input.wav")
	output := filepath.Join(root, "output.ogg")
	if err := os.WriteFile(input, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline := Pipeline{
		FFmpeg:        processor.ToolRunner{Path: ffmpeg},
		Probe:         mediavideo.FFProbe{Runner: processor.ToolRunner{Path: ffprobe}},
		MaxDurationMS: 1000, MaxOutputBytes: 1024,
	}
	policy := domain.DefaultPolicy()
	if _, _, err := pipeline.Process(context.Background(), input, output, policy, domain.MediaDetected{Kind: "audio", DurationMS: 100}); err != nil {
		t.Fatalf("process should not perform a second validation: %v", err)
	}
	if _, err := pipeline.Validate(context.Background(), output, policy.TargetAudio); err == nil {
		t.Fatal("manager validation should reject the invalid output")
	}
}

func writeAudioExecutable(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool.sh")
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
