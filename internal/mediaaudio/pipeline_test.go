package mediaaudio

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/mediavideo"
	"media-converter-v2/internal/processor"
)

func TestAudioPoliciesProduceCanonicalProfiles(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is unavailable")
	}
	root := t.TempDir()
	input := filepath.Join(root, "input.wav")
	cmd := exec.Command(ffmpeg, "-y", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=16000", "-t", "0.2", "-ac", "1", "-c:a", "pcm_s16le", "-ar", "16000", input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, output)
	}
	pipeline := Pipeline{FFmpeg: processor.ToolRunner{Path: ffmpeg}, Probe: mediavideo.FFProbe{Runner: processor.ToolRunner{Path: ffprobe}}, MaxOutputBytes: 10 << 20, FFmpegThreads: 1}
	detected, err := pipeline.Probe.Probe(context.Background(), input)
	if err != nil || detected.Kind != "audio" {
		t.Fatalf("detect input: %+v, %v", detected, err)
	}

	tests := []struct {
		policy    string
		extension string
		container string
		codec     string
		rate      int
	}{
		{policy: "voice_ogg_opus", extension: ".ogg", container: "ogg", codec: "opus", rate: 48000},
		{policy: "wav_pcm_s16le", extension: ".wav", container: "wav", codec: "pcm_s16le", rate: 16000},
		{policy: "m4a_aac_lc", extension: ".m4a", container: "m4a", codec: "aac", rate: 48000},
	}
	for _, test := range tests {
		t.Run(test.policy, func(t *testing.T) {
			output := filepath.Join(root, "output"+test.extension)
			policy := domain.DefaultPolicy()
			policy.TargetAudio = test.policy
			if _, _, err := pipeline.Process(context.Background(), input, output, policy, detected); err != nil {
				t.Fatal(err)
			}
			metadata, err := pipeline.Validate(context.Background(), output, test.policy)
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Extension != test.extension || metadata.AudioCodec != test.codec || metadata.SampleRate != test.rate || metadata.Channels != 1 {
				t.Fatalf("unexpected metadata: %+v", metadata)
			}
			if !strings.HasSuffix(output, test.extension) {
				t.Fatal("unexpected output extension")
			}
			if _, err := os.Stat(output); err != nil {
				t.Fatal(err)
			}
		})
	}
}
