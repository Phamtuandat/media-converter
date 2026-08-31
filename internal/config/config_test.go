package config

import "testing"

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LISTEN_ADDR", "100.100.100.10:8080")
	t.Setenv("MEDIA_SERVICE_TOKEN", "test-token")
}

func TestLoadDefaultsToLocalOutput(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MEDIA_OUTPUT_MODE", "")
	t.Setenv("WEBDAV_PUBLIC_BASE_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OutputMode != OutputModeLocal {
		t.Fatalf("output mode = %q, want %q", cfg.OutputMode, OutputModeLocal)
	}
}

func TestLoadAcceptsLoopbackListenAddr(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LISTEN_ADDR", "127.0.0.1:8080")

	if _, err := Load(); err != nil {
		t.Fatalf("loopback listen address rejected: %v", err)
	}
}

func TestLoadRejectsUnspecifiedListenAddr(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LISTEN_ADDR", "0.0.0.0:8080")

	if _, err := Load(); err == nil {
		t.Fatal("expected unspecified listen address to be rejected")
	}
}

func TestLoadWebDAVOutputRequiresDirectBaseURL(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MEDIA_OUTPUT_MODE", OutputModeWebDAV)
	t.Setenv("WEBDAV_PUBLIC_BASE_URL", "https://files.example.test/media-converter-v2/artifacts")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OutputMode != OutputModeWebDAV || cfg.WebDAVBaseURL == "" {
		t.Fatalf("unexpected WebDAV config: %+v", cfg)
	}

	t.Setenv("WEBDAV_PUBLIC_BASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected WebDAV base URL validation error")
	}
}

func TestLoadAcceptsSeparateLegacyArtifactRoot(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ARTIFACT_OUTPUT_ROOT", "/tmp/media-converter-v2/artifacts")
	t.Setenv("LEGACY_ARTIFACT_ROOT", "/tmp/media-converter-legacy/artifacts")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LegacyArtifactRoot != "/tmp/media-converter-legacy/artifacts" {
		t.Fatalf("legacy artifact root = %q", cfg.LegacyArtifactRoot)
	}
}

func TestLoadRejectsLegacyArtifactRootThatOverlapsOutput(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ARTIFACT_OUTPUT_ROOT", "/tmp/media-converter-v2/artifacts")
	t.Setenv("LEGACY_ARTIFACT_ROOT", "/tmp/media-converter-v2/artifacts")
	if _, err := Load(); err == nil {
		t.Fatal("expected overlapping legacy artifact root to be rejected")
	}
}

func TestLoadRejectsLegacyArtifactRootNestedUnderOutput(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ARTIFACT_OUTPUT_ROOT", "/tmp/media-converter-v2")
	t.Setenv("LEGACY_ARTIFACT_ROOT", "/tmp/media-converter-v2/legacy")
	if _, err := Load(); err == nil {
		t.Fatal("expected nested legacy artifact root to be rejected")
	}
}

func TestLoadRejectsUnknownOutputMode(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("MEDIA_OUTPUT_MODE", "remote")
	if _, err := Load(); err == nil {
		t.Fatal("expected output mode validation error")
	}
}
