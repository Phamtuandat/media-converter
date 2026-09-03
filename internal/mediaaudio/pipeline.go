package mediaaudio

import (
	"context"
	"fmt"
	"os"
	"strings"

	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/mediavideo"
	"media-converter-v2/internal/processor"
)

type Pipeline struct {
	FFmpeg         processor.ToolRunner
	Probe          mediavideo.FFProbe
	MaxDurationMS  int64
	MaxOutputBytes int64
	FFmpegThreads  int
}

func (p Pipeline) Process(ctx context.Context, input, output string, policy domain.Policy, detected domain.MediaDetected) (domain.Operation, domain.MediaDetected, error) {
	if detected.Kind != "audio" {
		return "", detected, domain.NewError("policy_input_kind_mismatch", "audio policy requires an audio-only input", "audio", false, nil)
	}
	if p.MaxDurationMS > 0 && detected.DurationMS > p.MaxDurationMS {
		return "", detected, domain.NewError("output_size_exceeded", "audio exceeds configured duration limit", "audio", false, nil)
	}
	args, err := encodeArgs(policy.TargetAudio, input, output, p.FFmpegThreads, policy.StripMetadata)
	if err != nil {
		return "", detected, err
	}
	if _, err := p.FFmpeg.RunWithOutputLimit(ctx, output, p.MaxOutputBytes, args...); err != nil {
		return "", detected, domain.NewError("converter_failed", "FFmpeg audio conversion failed", "audio", false, err)
	}
	return domain.OperationTranscoded, detected, nil
}

func encodeArgs(policy, input, output string, threads int, stripMetadata bool) ([]string, error) {
	args := []string{"-y", "-i", input, "-map", "0:a:0", "-vn"}
	if stripMetadata {
		args = append(args, "-map_metadata", "-1")
	}
	switch policy {
	case "voice_ogg_opus":
		args = append(args, "-c:a", "libopus", "-application", "voip", "-b:a", "32k", "-ar", "48000", "-ac", "1", "-f", "ogg", output)
	case "wav_pcm_s16le":
		args = append(args, "-c:a", "pcm_s16le", "-ar", "16000", "-ac", "1", "-f", "wav", output)
	case "m4a_aac_lc":
		args = append(args, "-c:a", "aac", "-profile:a", "aac_low", "-b:a", "96k", "-ar", "48000", "-ac", "1", "-movflags", "+faststart", output)
	default:
		return nil, domain.NewError("request_invalid", "unsupported audio policy", "audio", false, nil)
	}
	if threads > 0 {
		args = append(args[:len(args)-1], "-threads", fmt.Sprint(threads), args[len(args)-1])
	}
	return args, nil
}

func (p Pipeline) Validate(ctx context.Context, path, policy string) (domain.OutputMetadata, error) {
	m, err := p.Probe.Probe(ctx, path)
	if err != nil {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "audio output could not be probed", "validate", false, err)
	}
	if m.Kind != "audio" || m.StreamCount != 1 || m.Channels != 1 {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "audio output is not a mono audio stream", "validate", false, nil)
	}
	wantContainer, wantCodec, wantRate := profile(policy)
	if m.Container != wantContainer || !strings.EqualFold(m.AudioCodec, wantCodec) || m.SampleRate != wantRate {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "audio output does not satisfy canonical policy", "validate", false, nil)
	}
	if policy == "m4a_aac_lc" && m.AudioProfile != "" && !strings.EqualFold(m.AudioProfile, "LC") {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "audio output is not AAC-LC", "validate", false, nil)
	}
	info, err := os.Stat(path)
	if err != nil {
		return domain.OutputMetadata{}, domain.NewError("output_validation_failed", "audio output is missing", "validate", false, err)
	}
	extension, mime := outputType(policy)
	return domain.OutputMetadata{
		Extension: extension, MIME: mime, Size: info.Size(), DurationMS: m.DurationMS,
		AudioCodec: m.AudioCodec, AudioProfile: m.AudioProfile, SampleRate: m.SampleRate, Channels: m.Channels,
	}, nil
}

func profile(policy string) (container, codec string, sampleRate int) {
	switch policy {
	case "wav_pcm_s16le":
		return "wav", "pcm_s16le", 16000
	case "m4a_aac_lc":
		return "m4a", "aac", 48000
	default:
		return "ogg", "opus", 48000
	}
}

func outputType(policy string) (extension, mime string) {
	switch policy {
	case "wav_pcm_s16le":
		return ".wav", "audio/wav"
	case "m4a_aac_lc":
		return ".m4a", "audio/mp4"
	default:
		return ".ogg", "audio/ogg"
	}
}
