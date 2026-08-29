package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr               string
	BearerToken              string
	PublicBaseURL            string
	OutputMode               string
	WebDAVBaseURL            string
	InputRoot                string
	OutputRoot               string
	LegacyArtifactRoot       string
	StateRoot                string
	CacheRoot                string
	WorkRoot                 string
	FFmpegPath               string
	FFprobePath              string
	MagickPath               string
	ProcessorVersion         string
	PolicyVersion            string
	ImageWorkers             int
	VideoWorkers             int
	JobWorkers               int
	QueueSize                int
	MaxInputBytes            int64
	MaxOutputBytes           int64
	MaxWidth                 int
	MaxHeight                int
	MaxPixels                int64
	MaxDuration              time.Duration
	JobTimeout               time.Duration
	ShutdownTimeout          time.Duration
	FFmpegThreads            int
	MaxItemsPerJob           int
	MaxConcurrentItemsPerJob int
	MaxAggregateInputBytes   int64
	MaxAggregatePixels       int64
	MaxAggregateDuration     time.Duration
	WorkspaceRetention       time.Duration
	StateRetention           time.Duration
	CacheRetention           time.Duration
	ArtifactRetention        time.Duration
	StagingRetention         time.Duration
	JanitorInterval          time.Duration
	HTTPReadTimeout          time.Duration
	HTTPWriteTimeout         time.Duration
	HTTPIdleTimeout          time.Duration
	ToolFingerprint          string
	ToolVersions             map[string]string
	ToolAvailability         map[string]bool
	ImageFormats             map[string]bool
}

const (
	OutputModeLocal  = "local"
	OutputModeWebDAV = "webdav"
)

func Load() (Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Config{}, err
	}
	c := Config{
		ListenAddr:               os.Getenv("LISTEN_ADDR"),
		BearerToken:              os.Getenv("MEDIA_SERVICE_TOKEN"),
		PublicBaseURL:            strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL")), "/"),
		OutputMode:               strings.ToLower(strings.TrimSpace(env("MEDIA_OUTPUT_MODE", OutputModeLocal))),
		WebDAVBaseURL:            strings.TrimRight(strings.TrimSpace(env("WEBDAV_PUBLIC_BASE_URL", os.Getenv("WEBDAV_BASE_URL"))), "/"),
		InputRoot:                env("ARTIFACT_INPUT_ROOT", filepath.Join(cwd, "data", "staging")),
		OutputRoot:               env("ARTIFACT_OUTPUT_ROOT", filepath.Join(cwd, "data", "artifacts")),
		LegacyArtifactRoot:       strings.TrimSpace(env("LEGACY_ARTIFACT_ROOT", os.Getenv("LEGACY_ARTIFACT_OUTPUT_ROOT"))),
		StateRoot:                env("JOB_STATE_ROOT", filepath.Join(cwd, "data", "state")),
		CacheRoot:                env("CACHE_ROOT", filepath.Join(cwd, "data", "cache")),
		WorkRoot:                 env("WORK_ROOT", filepath.Join(cwd, "data", "work")),
		FFmpegPath:               env("FFMPEG_PATH", "ffmpeg"),
		FFprobePath:              env("FFPROBE_PATH", "ffprobe"),
		MagickPath:               env("MAGICK_PATH", "magick"),
		ProcessorVersion:         env("PROCESSOR_VERSION", "1.0.0"),
		PolicyVersion:            env("POLICY_VERSION", "media-v1.1"),
		ImageWorkers:             intEnv("IMAGE_WORKERS", 4),
		VideoWorkers:             intEnv("VIDEO_WORKERS", 3),
		JobWorkers:               intEnv("JOB_WORKERS", 4),
		QueueSize:                intEnv("QUEUE_SIZE", 32),
		MaxInputBytes:            int64Env("MAX_INPUT_BYTES", 1<<30),
		MaxOutputBytes:           int64Env("MAX_OUTPUT_BYTES", 2<<30),
		MaxWidth:                 intEnv("MAX_WIDTH", 12000),
		MaxHeight:                intEnv("MAX_HEIGHT", 12000),
		MaxPixels:                int64Env("MAX_PIXELS", 12000*12000),
		MaxDuration:              durationEnv("MAX_DURATION", 30*time.Minute),
		JobTimeout:               durationEnv("JOB_TIMEOUT", 45*time.Minute),
		ShutdownTimeout:          durationEnv("SHUTDOWN_TIMEOUT", 30*time.Second),
		FFmpegThreads:            intEnv("FFMPEG_THREADS", 3),
		MaxItemsPerJob:           intEnv("MAX_ITEMS_PER_JOB", 64),
		MaxConcurrentItemsPerJob: intEnv("MAX_CONCURRENT_ITEMS_PER_JOB", 4),
		MaxAggregateInputBytes:   int64Env("MAX_AGGREGATE_INPUT_BYTES", 4<<30),
		MaxAggregatePixels:       int64Env("MAX_AGGREGATE_PIXELS", 8*12000*12000),
		MaxAggregateDuration:     durationEnv("MAX_AGGREGATE_DURATION", 4*time.Hour),
		WorkspaceRetention:       durationEnv("WORKSPACE_RETENTION", 24*time.Hour),
		StateRetention:           durationEnv("STATE_RETENTION", 7*24*time.Hour),
		CacheRetention:           durationEnv("CACHE_RETENTION", 7*24*time.Hour),
		ArtifactRetention:        durationEnv("ARTIFACT_RETENTION", 14*24*time.Hour),
		StagingRetention:         durationEnv("STAGING_RETENTION", 7*24*time.Hour),
		JanitorInterval:          durationEnv("JANITOR_INTERVAL", time.Hour),
		HTTPReadTimeout:          durationEnv("HTTP_READ_TIMEOUT", 30*time.Second),
		HTTPWriteTimeout:         durationEnv("HTTP_WRITE_TIMEOUT", 2*time.Minute),
		HTTPIdleTimeout:          durationEnv("HTTP_IDLE_TIMEOUT", 2*time.Minute),
		ToolFingerprint:          env("TOOL_FINGERPRINT", ""),
	}
	if c.ListenAddr == "" {
		return Config{}, fmt.Errorf("LISTEN_ADDR is required; bind the service to its Tailscale address")
	}
	if c.BearerToken == "" {
		return Config{}, fmt.Errorf("MEDIA_SERVICE_TOKEN is required")
	}
	if c.OutputMode != OutputModeLocal && c.OutputMode != OutputModeWebDAV {
		return Config{}, fmt.Errorf("MEDIA_OUTPUT_MODE must be %q or %q", OutputModeLocal, OutputModeWebDAV)
	}
	if err := validateBaseURL("PUBLIC_BASE_URL", c.PublicBaseURL, false); err != nil {
		return Config{}, err
	}
	if c.LegacyArtifactRoot != "" {
		legacy, err := filepath.Abs(c.LegacyArtifactRoot)
		if err != nil {
			return Config{}, fmt.Errorf("LEGACY_ARTIFACT_ROOT is invalid: %w", err)
		}
		output, err := filepath.Abs(c.OutputRoot)
		if err != nil || pathsOverlap(legacy, output) || pathsOverlap(legacy, c.InputRoot) {
			return Config{}, fmt.Errorf("LEGACY_ARTIFACT_ROOT must be separate from ARTIFACT_OUTPUT_ROOT")
		}
		c.LegacyArtifactRoot = legacy
	}
	if c.OutputMode == OutputModeWebDAV {
		if err := validateBaseURL("WEBDAV_PUBLIC_BASE_URL", c.WebDAVBaseURL, true); err != nil {
			return Config{}, err
		}
	}
	if c.ImageWorkers < 1 || c.VideoWorkers < 1 || c.JobWorkers < 1 || c.QueueSize < 1 {
		return Config{}, fmt.Errorf("worker and queue settings must be positive")
	}
	if c.MaxInputBytes <= 0 || c.MaxOutputBytes <= 0 || c.MaxPixels <= 0 {
		return Config{}, fmt.Errorf("media limits must be positive")
	}
	if c.MaxItemsPerJob < 1 || c.MaxConcurrentItemsPerJob < 1 || c.MaxAggregateInputBytes <= 0 || c.MaxAggregatePixels <= 0 || c.MaxAggregateDuration <= 0 {
		return Config{}, fmt.Errorf("job resource limits must be positive")
	}
	if c.WorkspaceRetention <= 0 || c.StateRetention <= 0 || c.CacheRetention <= 0 || c.ArtifactRetention <= 0 || c.StagingRetention <= 0 || c.JanitorInterval <= 0 {
		return Config{}, fmt.Errorf("retention settings must be positive")
	}
	if err := validateListenAddr(c.ListenAddr); err != nil {
		return Config{}, err
	}
	return c, nil
}

func validateBaseURL(name, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s is required when MEDIA_OUTPUT_MODE=webdav", name)
		}
		return nil
	}
	base, err := url.Parse(value)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL without query or fragment", name)
	}
	return nil
}

func validateListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("LISTEN_ADDR must be host:port on a private/Tailscale interface")
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || (!ip.IsPrivate() && !isTailscaleAddress(ip)) {
		return fmt.Errorf("LISTEN_ADDR must use a non-loopback private/Tailscale address")
	}
	return nil
}

func isTailscaleAddress(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
	}
	return false
}

type ToolStatus struct {
	Name, Path, Version string
	Available           bool
	Error               string
	Capabilities        map[string]bool
}

func CheckImageDelegates(path string) ToolStatus {
	capabilities := map[string]bool{
		"jpeg": false,
		"png":  false,
		"webp": false,
		"heic": false,
		"heif": false,
		"avif": false,
	}
	status := ToolStatus{Name: "imagemagick-delegates", Path: path, Capabilities: capabilities}
	resolved, err := exec.LookPath(path)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	out, err := exec.Command(resolved, "identify", "-list", "format").Output()
	if err != nil {
		status.Error = err.Error()
		return status
	}
	text := strings.ToUpper(string(out))
	capabilities["jpeg"] = strings.Contains(text, "JPEG")
	capabilities["png"] = strings.Contains(text, "PNG")
	capabilities["webp"] = strings.Contains(text, "WEBP")
	capabilities["heic"] = strings.Contains(text, "HEIC") || strings.Contains(text, "HEIF")
	capabilities["heif"] = capabilities["heic"]
	capabilities["avif"] = strings.Contains(text, "AVIF")
	missing := make([]string, 0, 3)
	for _, format := range []string{"HEIC", "AVIF", "WEBP"} {
		if !strings.Contains(text, format) {
			missing = append(missing, format)
		}
	}
	if len(missing) > 0 {
		status.Error = "missing delegates: " + strings.Join(missing, ", ")
		return status
	}
	status.Available = true
	return status
}

func CheckTool(path string, name string) ToolStatus {
	resolved, err := exec.LookPath(path)
	if err != nil {
		return ToolStatus{Name: name, Path: path, Available: false, Error: err.Error()}
	}
	cmd := exec.Command(resolved, "-version")
	out, err := cmd.Output()
	if err != nil {
		return ToolStatus{Name: name, Path: resolved, Available: false, Error: err.Error()}
	}
	version := string(out)
	if len(version) > 120 {
		version = version[:120]
	}
	return ToolStatus{Name: name, Path: resolved, Version: version, Available: true}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func pathsOverlap(first, second string) bool {
	return pathWithin(first, second) || pathWithin(second, first)
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
func intEnv(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
func int64Env(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
func durationEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
