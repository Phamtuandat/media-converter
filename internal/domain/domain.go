package domain

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

type JobRequest struct {
	JobID  string `json:"job_id"`
	Items  []Item `json:"items"`
	Policy Policy `json:"policy"`
}

func (r JobRequest) Normalized() JobRequest {
	if r.Policy == (Policy{}) {
		r.Policy = DefaultPolicy()
		return r
	}
	defaults := DefaultPolicy()
	if r.Policy.TargetImage == "" {
		r.Policy.TargetImage = defaults.TargetImage
	}
	if r.Policy.TargetVideo == "" {
		r.Policy.TargetVideo = defaults.TargetVideo
	}
	if r.Policy.TargetAudio == "" {
		r.Policy.TargetAudio = defaults.TargetAudio
	}
	if r.Policy.AlphaBackground == "" {
		r.Policy.AlphaBackground = defaults.AlphaBackground
	}
	return r
}

func (r JobRequest) ValidatePolicy() bool {
	if r.Policy.TargetImage != "zalo_jpeg" || r.Policy.TargetVideo != "zalo_mp4_h264_aac_faststart" {
		return false
	}
	if r.Policy.TargetAudio != "voice_ogg_opus" && r.Policy.TargetAudio != "wav_pcm_s16le" && r.Policy.TargetAudio != "m4a_aac_lc" {
		return false
	}
	background := strings.ToUpper(r.Policy.AlphaBackground)
	return len(background) == 7 && background[0] == '#' && regexp.MustCompile(`^[0-9A-F]{6}$`).MatchString(background[1:])
}

func IsSHA256(value string) bool { return sha256Pattern.MatchString(value) }

type Item struct {
	ID             string `json:"id"`
	ArtifactID     string `json:"artifact_id"`
	DeclaredKind   string `json:"declared_kind,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

type Policy struct {
	TargetImage         string `json:"target_image"`
	TargetVideo         string `json:"target_video"`
	TargetAudio         string `json:"target_audio"`
	StripMetadata       bool   `json:"strip_metadata"`
	GenerateThumbnail   bool   `json:"generate_thumbnail"`
	AlphaBackground     string `json:"alpha_background"`
	StrictFormatMatch   bool   `json:"strict_format_match"`
	IncludeDownloadURLs bool   `json:"include_download_urls,omitempty"`
}

func (p *Policy) UnmarshalJSON(data []byte) error {
	type rawPolicy struct {
		TargetImage         string `json:"target_image"`
		TargetVideo         string `json:"target_video"`
		TargetAudio         string `json:"target_audio"`
		StripMetadata       *bool  `json:"strip_metadata"`
		GenerateThumbnail   *bool  `json:"generate_thumbnail"`
		AlphaBackground     string `json:"alpha_background"`
		StrictFormatMatch   bool   `json:"strict_format_match"`
		IncludeDownloadURLs bool   `json:"include_download_urls"`
	}
	var raw rawPolicy
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	defaults := DefaultPolicy()
	p.TargetImage, p.TargetVideo, p.TargetAudio, p.AlphaBackground = raw.TargetImage, raw.TargetVideo, raw.TargetAudio, raw.AlphaBackground
	if p.TargetImage == "" {
		p.TargetImage = defaults.TargetImage
	}
	if p.TargetVideo == "" {
		p.TargetVideo = defaults.TargetVideo
	}
	if p.TargetAudio == "" {
		p.TargetAudio = defaults.TargetAudio
	}
	if p.AlphaBackground == "" {
		p.AlphaBackground = defaults.AlphaBackground
	}
	p.StripMetadata = defaults.StripMetadata
	p.GenerateThumbnail = defaults.GenerateThumbnail
	if raw.StripMetadata != nil {
		p.StripMetadata = *raw.StripMetadata
	}
	if raw.GenerateThumbnail != nil {
		p.GenerateThumbnail = *raw.GenerateThumbnail
	}
	p.StrictFormatMatch = raw.StrictFormatMatch
	p.IncludeDownloadURLs = raw.IncludeDownloadURLs
	return nil
}

func DefaultPolicy() Policy {
	return Policy{
		TargetImage:       "zalo_jpeg",
		TargetVideo:       "zalo_mp4_h264_aac_faststart",
		TargetAudio:       "voice_ogg_opus",
		StripMetadata:     true,
		GenerateThumbnail: true,
		AlphaBackground:   "#FFFFFF",
	}
}

type JobState string

const (
	JobQueued     JobState = "queued"
	JobProcessing JobState = "processing"
	JobCompleted  JobState = "completed"
)

type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomePartial  Outcome = "partial_success"
	OutcomeRejected Outcome = "rejected"
	OutcomeFailed   Outcome = "failed"
)

type ItemStatus string

const (
	ItemSuccess  ItemStatus = "success"
	ItemRejected ItemStatus = "rejected"
	ItemFailed   ItemStatus = "failed"
)

type Operation string

const (
	OperationPassthrough Operation = "passthrough"
	OperationNormalized  Operation = "normalized"
	OperationRemuxed     Operation = "remuxed"
	OperationTranscoded  Operation = "transcoded"
)

type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type MediaDetected struct {
	Kind         string `json:"kind"`
	Format       string `json:"format,omitempty"`
	Container    string `json:"container,omitempty"`
	MIME         string `json:"mime"`
	Animated     bool   `json:"animated,omitempty"`
	VideoCodec   string `json:"video_codec,omitempty"`
	AudioCodec   string `json:"audio_codec,omitempty"`
	AudioProfile string `json:"audio_profile,omitempty"`
	SampleRate   int    `json:"sample_rate,omitempty"`
	Channels     int    `json:"channels,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	PixelFormat  string `json:"pixel_format,omitempty"`
	Rotation     int    `json:"rotation,omitempty"`
	StreamCount  int    `json:"stream_count,omitempty"`
	Faststart    bool   `json:"faststart,omitempty"`
	HasAudio     bool   `json:"has_audio,omitempty"`
}

type InputMetadata struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type OutputMetadata struct {
	ArtifactID   string `json:"artifact_id"`
	Filename     string `json:"filename"`
	Extension    string `json:"extension"`
	MIME         string `json:"mime"`
	Size         int64  `json:"size"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	ColorSpace   string `json:"color_space,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	VideoCodec   string `json:"video_codec,omitempty"`
	AudioCodec   string `json:"audio_codec,omitempty"`
	AudioProfile string `json:"audio_profile,omitempty"`
	SampleRate   int    `json:"sample_rate,omitempty"`
	Channels     int    `json:"channels,omitempty"`
	PixelFormat  string `json:"pixel_format,omitempty"`
	Faststart    bool   `json:"faststart,omitempty"`
	SHA256       string `json:"sha256"`
}

type ThumbnailMetadata struct {
	ArtifactID string `json:"artifact_id"`
	Filename   string `json:"filename"`
	Extension  string `json:"extension"`
	MIME       string `json:"mime"`
	Size       int64  `json:"size"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	SHA256     string `json:"sha256"`
}

type ItemResult struct {
	ID        string             `json:"id"`
	Status    ItemStatus         `json:"status"`
	Operation Operation          `json:"operation,omitempty"`
	Input     *InputMetadata     `json:"input,omitempty"`
	Detected  *MediaDetected     `json:"detected,omitempty"`
	Output    *OutputMetadata    `json:"output,omitempty"`
	Thumbnail *ThumbnailMetadata `json:"thumbnail,omitempty"`
	Transform string             `json:"transform,omitempty"`
	Warnings  []Warning          `json:"warnings,omitempty"`
	Error     *ProcessingError   `json:"error,omitempty"`
	CacheKey  string             `json:"-"`
}

type ItemProgressState string

const (
	ProgressPending    ItemProgressState = "pending"
	ProgressProcessing ItemProgressState = "processing"
	ProgressCommitted  ItemProgressState = "committed"
	ProgressRejected   ItemProgressState = "rejected"
	ProgressFailed     ItemProgressState = "failed"
)

// ItemProgress is durable processing intent and commit metadata. It lets
// restart recovery validate an already committed artifact without rerunning a
// converter after a job result write failure.
type ItemProgress struct {
	ID                  string            `json:"id"`
	State               ItemProgressState `json:"state"`
	Kind                string            `json:"kind,omitempty"`
	Input               *InputMetadata    `json:"input,omitempty"`
	Detected            *MediaDetected    `json:"detected,omitempty"`
	Operation           Operation         `json:"operation,omitempty"`
	PlannedOutputID     string            `json:"planned_output_id,omitempty"`
	PlannedThumbnailID  string            `json:"planned_thumbnail_id,omitempty"`
	OutputArtifactID    string            `json:"output_artifact_id,omitempty"`
	ThumbnailArtifactID string            `json:"thumbnail_artifact_id,omitempty"`
	Result              *ItemResult       `json:"result,omitempty"`
}

type ProcessingError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Stage     string `json:"stage,omitempty"`
}

type JobResult struct {
	Outcome  Outcome      `json:"outcome"`
	Items    []ItemResult `json:"items"`
	Warnings []Warning    `json:"warnings,omitempty"`
}

type ProcessorInfo struct {
	Version      string            `json:"version"`
	Policy       string            `json:"policy_version"`
	ToolVersions map[string]string `json:"tool_versions,omitempty"`
}

type JobRecord struct {
	JobID       string         `json:"job_id"`
	Request     JobRequest     `json:"request"`
	RequestHash string         `json:"request_hash"`
	State       JobState       `json:"state"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Progress    []ItemProgress `json:"progress,omitempty"`
	Result      *JobResult     `json:"result,omitempty"`
	Processor   ProcessorInfo  `json:"processor"`
}

func (r JobResult) Aggregate() Outcome {
	var success, rejected, failed int
	for _, item := range r.Items {
		switch item.Status {
		case ItemSuccess:
			success++
		case ItemRejected:
			rejected++
		case ItemFailed:
			failed++
		}
	}
	if success == len(r.Items) {
		return OutcomeSuccess
	}
	if success > 0 {
		return OutcomePartial
	}
	if failed > 0 {
		return OutcomeFailed
	}
	return OutcomeRejected
}
