package httpapi

import (
	"net/http"
	"time"

	"media-converter-v2/internal/config"
	"media-converter-v2/internal/domain"
)

const discoveryAPIVersion = "v1"

type discoveryManifest struct {
	Service        string              `json:"service"`
	Version        string              `json:"version"`
	APIVersion     string              `json:"api_version"`
	Authentication discoveryAuth       `json:"authentication"`
	Endpoints      discoveryEndpoints  `json:"endpoints"`
	JobModel       discoveryJobModel   `json:"job_model"`
	ItemOutcomes   []string            `json:"item_outcomes"`
	Operations     []string            `json:"operations"`
	MediaContract  discoveryMedia      `json:"media_contract"`
	Workflow       discoveryWorkflow   `json:"workflow"`
	AgentGuidance  discoveryAgentGuide `json:"agent_guidance"`
}

type discoveryAuth struct {
	Type string `json:"type"`
}

type discoveryEndpoints struct {
	UploadArtifact string `json:"upload_artifact"`
	CreateJob      string `json:"create_job"`
	GetJob         string `json:"get_job"`
	GetDownloads   string `json:"get_downloads"`
	GetArtifact    string `json:"get_artifact"`
	Capabilities   string `json:"capabilities"`
	HealthLive     string `json:"health_live"`
	HealthReady    string `json:"health_ready"`
	Metrics        string `json:"metrics"`
	OpenAPI        string `json:"openapi"`
}

type discoveryJobModel struct {
	Async  bool     `json:"async"`
	States []string `json:"states"`
}

type discoveryMedia struct {
	ImageInputs []string           `json:"image_inputs"`
	VideoInputs []string           `json:"video_inputs"`
	AudioInputs []string           `json:"audio_inputs"`
	Canonical   discoveryCanonical `json:"canonical_output"`
}

type discoveryCanonical struct {
	Image     discoveryImageOutput   `json:"image"`
	Video     discoveryVideoOutput   `json:"video"`
	Audio     []discoveryAudioOutput `json:"audio"`
	Thumbnail discoveryImageOutput   `json:"thumbnail"`
}

type discoveryAudioOutput struct {
	Policy     string `json:"policy"`
	MIME       string `json:"mime"`
	Extension  string `json:"extension"`
	Container  string `json:"container"`
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

type discoveryImageOutput struct {
	MIME      string `json:"mime"`
	Extension string `json:"extension"`
}

type discoveryVideoOutput struct {
	MIME        string `json:"mime"`
	Extension   string `json:"extension"`
	VideoCodec  string `json:"video_codec"`
	PixelFormat string `json:"pixel_format"`
	AudioCodec  string `json:"audio_codec"`
	AudioPolicy string `json:"audio_policy"`
	Faststart   bool   `json:"faststart"`
}

type discoveryWorkflow struct {
	Upload    string `json:"upload"`
	Submit    string `json:"submit"`
	Status    string `json:"status"`
	Downloads string `json:"downloads"`
	Artifact  string `json:"artifact"`
}

type discoveryAgentGuide struct {
	SubmitOnlyWhenReady               bool     `json:"submit_only_when_ready"`
	DoNotSendFilesystemPaths          bool     `json:"do_not_send_filesystem_paths"`
	UseArtifactIDOnly                 bool     `json:"use_artifact_id_only"`
	DoNotAssumeExtensionAuthoritative bool     `json:"do_not_assume_extension_is_authoritative"`
	UseOnlyArtifactsFromCompletedJobs bool     `json:"use_only_artifacts_from_completed_jobs"`
	Steps                             []string `json:"steps"`
}

type capabilityResponse struct {
	ProcessorVersion string                  `json:"processor_version"`
	PolicyVersion    string                  `json:"policy_version"`
	Features         capabilityFeatures      `json:"features"`
	AudioPolicies    []capabilityAudioPolicy `json:"audio_policies"`
	Runtime          capabilityRuntime       `json:"runtime"`
	Formats          capabilityFormats       `json:"formats"`
	Workers          capabilityWorkers       `json:"workers"`
	Limits           capabilityLimits        `json:"limits"`
}

type capabilityFeatures struct {
	ArtifactUpload  bool   `json:"artifact_upload"`
	Staging         bool   `json:"staging"`
	DownloadURLs    bool   `json:"download_urls"`
	DownloadURLMode string `json:"download_url_mode"`
}

type capabilityAudioPolicy struct {
	Policy     string `json:"policy"`
	MIME       string `json:"mime"`
	Extension  string `json:"extension"`
	Container  string `json:"container"`
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
}

type capabilityRuntime struct {
	FFmpeg      bool `json:"ffmpeg"`
	FFprobe     bool `json:"ffprobe"`
	ImageMagick bool `json:"imagemagick"`
}

type capabilityFormats struct {
	JPEG      bool `json:"jpeg"`
	PNG       bool `json:"png"`
	WebP      bool `json:"webp"`
	HEIC      bool `json:"heic"`
	HEIF      bool `json:"heif"`
	AVIF      bool `json:"avif"`
	MOV       bool `json:"mov"`
	MP4       bool `json:"mp4"`
	M4V       bool `json:"m4v"`
	QuickTime bool `json:"quicktime"`
	OGG       bool `json:"ogg"`
	WAV       bool `json:"wav"`
	M4A       bool `json:"m4a"`
}

type capabilityWorkers struct {
	ImageConcurrency int `json:"image_concurrency"`
	VideoConcurrency int `json:"video_concurrency"`
	JobConcurrency   int `json:"job_concurrency"`
	QueueCapacity    int `json:"queue_capacity"`
}

type capabilityLimits struct {
	MaxInputBytes            int64 `json:"max_input_bytes"`
	MaxOutputBytes           int64 `json:"max_output_bytes"`
	MaxVideoDurationSeconds  int64 `json:"max_video_duration_seconds"`
	MaxWidth                 int   `json:"max_width"`
	MaxHeight                int   `json:"max_height"`
	MaxPixels                int64 `json:"max_pixels"`
	MaxItemsPerJob           int   `json:"max_items_per_job"`
	MaxConcurrentItemsPerJob int   `json:"max_concurrent_items_per_job"`
}

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	writeJSON(w, http.StatusOK, discoveryManifestFor(s.cfg))
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, capabilitiesFor(s.cfg))
}

func discoveryManifestFor(cfg config.Config) discoveryManifest {
	return discoveryManifest{
		Service:        "media-converter",
		Version:        cfg.ProcessorVersion,
		APIVersion:     discoveryAPIVersion,
		Authentication: discoveryAuth{Type: "bearer"},
		Endpoints: discoveryEndpoints{
			UploadArtifact: "/v1/artifacts",
			CreateJob:      "/v1/jobs", GetJob: "/v1/jobs/{job_id}", GetDownloads: "/v1/jobs/{job_id}/downloads", GetArtifact: "/v1/artifacts/{artifact_id}",
			Capabilities: "/v1/capabilities", HealthLive: "/health/live", HealthReady: "/health/ready",
			Metrics: "/metrics", OpenAPI: "/openapi.json",
		},
		JobModel:     discoveryJobModel{Async: true, States: []string{string(domain.JobQueued), string(domain.JobProcessing), string(domain.JobCompleted)}},
		ItemOutcomes: []string{string(domain.ItemSuccess), string(domain.ItemRejected), string(domain.ItemFailed)},
		Operations:   []string{string(domain.OperationPassthrough), string(domain.OperationNormalized), string(domain.OperationRemuxed), string(domain.OperationTranscoded)},
		MediaContract: discoveryMedia{
			ImageInputs: []string{"jpeg", "png", "webp", "heic", "heif", "avif"},
			VideoInputs: []string{"mov", "quicktime", "mp4", "m4v"},
			AudioInputs: []string{"ogg", "wav", "m4a"},
			Canonical: discoveryCanonical{
				Image: discoveryImageOutput{MIME: "image/jpeg", Extension: ".jpg"},
				Video: discoveryVideoOutput{MIME: "video/mp4", Extension: ".mp4", VideoCodec: "h264", PixelFormat: "yuv420p", AudioCodec: "aac", AudioPolicy: "aac_if_present", Faststart: true},
				Audio: []discoveryAudioOutput{
					{Policy: "voice_ogg_opus", MIME: "audio/ogg", Extension: ".ogg", Container: "ogg", Codec: "opus", SampleRate: 48000, Channels: 1},
					{Policy: "wav_pcm_s16le", MIME: "audio/wav", Extension: ".wav", Container: "wav", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1},
					{Policy: "m4a_aac_lc", MIME: "audio/mp4", Extension: ".m4a", Container: "m4a", Codec: "aac_lc", SampleRate: 48000, Channels: 1},
				},
				Thumbnail: discoveryImageOutput{MIME: "image/jpeg", Extension: ".jpg"},
			},
		},
		Workflow: discoveryWorkflow{Upload: "POST /v1/artifacts", Submit: "POST /v1/jobs", Status: "GET /v1/jobs/{job_id}", Downloads: "GET /v1/jobs/{job_id}/downloads", Artifact: "GET /v1/artifacts/{artifact_id}"},
		AgentGuidance: discoveryAgentGuide{
			SubmitOnlyWhenReady: true, DoNotSendFilesystemPaths: true, UseArtifactIDOnly: true,
			DoNotAssumeExtensionAuthoritative: true, UseOnlyArtifactsFromCompletedJobs: true,
			Steps: []string{
				"Read the discovery manifest.",
				"Check capabilities when runtime support is needed.",
				"Check readiness before submitting a job.",
				"Fetch source bytes and POST them to /v1/artifacts with Content-Type and X-Filename.",
				"POST a job using the returned staging artifact_id and receive 202 Accepted with a job_id.",
				"GET the job until its state is completed.",
				"For jobs submitted with policy.include_download_urls=true, GET /v1/jobs/{job_id}/downloads after completion to retrieve download URLs.",
				"Use each returned download_url verbatim when present; otherwise use the returned artifact ID through the documented artifact endpoint.",
			},
		},
	}
}

func capabilitiesFor(cfg config.Config) capabilityResponse {
	formats := cfg.ImageFormats
	imageMagickAvailable := cfg.ToolAvailability != nil && cfg.ToolAvailability["imagemagick"]
	imageCapability := func(name string) bool { return imageMagickAvailable && formats != nil && formats[name] }
	videoCapability := cfg.ToolAvailability != nil && cfg.ToolAvailability["ffmpeg"] && cfg.ToolAvailability["ffprobe"]
	audioPolicies := []capabilityAudioPolicy(nil)
	if videoCapability {
		audioPolicies = []capabilityAudioPolicy{
			{Policy: "voice_ogg_opus", MIME: "audio/ogg", Extension: ".ogg", Container: "ogg", Codec: "opus", SampleRate: 48000, Channels: 1},
			{Policy: "wav_pcm_s16le", MIME: "audio/wav", Extension: ".wav", Container: "wav", Codec: "pcm_s16le", SampleRate: 16000, Channels: 1},
			{Policy: "m4a_aac_lc", MIME: "audio/mp4", Extension: ".m4a", Container: "m4a", Codec: "aac_lc", SampleRate: 48000, Channels: 1},
		}
	}
	return capabilityResponse{
		ProcessorVersion: cfg.ProcessorVersion,
		PolicyVersion:    cfg.PolicyVersion,
		Features:         capabilityFeatures{ArtifactUpload: true, Staging: true, DownloadURLs: true, DownloadURLMode: downloadURLMode(cfg)},
		AudioPolicies:    audioPolicies,
		Runtime: capabilityRuntime{
			FFmpeg:      cfg.ToolAvailability != nil && cfg.ToolAvailability["ffmpeg"],
			FFprobe:     cfg.ToolAvailability != nil && cfg.ToolAvailability["ffprobe"],
			ImageMagick: cfg.ToolAvailability != nil && cfg.ToolAvailability["imagemagick"],
		},
		Formats: capabilityFormats{
			JPEG: imageCapability("jpeg"), PNG: imageCapability("png"), WebP: imageCapability("webp"),
			HEIC: imageCapability("heic"), HEIF: imageCapability("heif"), AVIF: imageCapability("avif"),
			MOV: videoCapability, MP4: videoCapability, M4V: videoCapability, QuickTime: videoCapability,
			OGG: videoCapability, WAV: videoCapability, M4A: videoCapability,
		},
		Workers: capabilityWorkers{ImageConcurrency: cfg.ImageWorkers, VideoConcurrency: cfg.VideoWorkers, JobConcurrency: cfg.JobWorkers, QueueCapacity: cfg.QueueSize},
		Limits: capabilityLimits{
			MaxInputBytes: cfg.MaxInputBytes, MaxOutputBytes: cfg.MaxOutputBytes,
			MaxVideoDurationSeconds: int64(cfg.MaxDuration / time.Second), MaxWidth: cfg.MaxWidth, MaxHeight: cfg.MaxHeight,
			MaxPixels: cfg.MaxPixels, MaxItemsPerJob: cfg.MaxItemsPerJob, MaxConcurrentItemsPerJob: cfg.MaxConcurrentItemsPerJob,
		},
	}
}

func downloadURLMode(cfg config.Config) string {
	if configuredDownloadBaseURL(cfg) != "" {
		return "absolute"
	}
	return "relative"
}
