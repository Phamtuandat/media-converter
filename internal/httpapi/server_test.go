package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"log/slog"

	"media-converter-v2/internal/config"
	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/job"
	"media-converter-v2/internal/state"
	"media-converter-v2/internal/storage"
)

func TestJobsRejectsTrailingJSON(t *testing.T) {
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BearerToken: "secret", QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)
	server.SetReady(true)
	body := `{"job_id":"trailing","items":[{"id":"one","artifact_id":"input"}]} {}`
	request := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d", recorder.Code)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
}

func TestHealthAndAuth(t *testing.T) {
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BearerToken: "secret", QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetReady(true)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	resp, err := http.Get(httpServer.URL + "/health/live")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live status %d", resp.StatusCode)
	}
	resp, err = http.Get(httpServer.URL + "/v1/jobs/missing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth status %d", resp.StatusCode)
	}
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"ready"`) {
		t.Fatalf("unexpected readiness response: %d %s", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("metrics should require auth, got %d", recorder.Code)
	}
	request.Header.Set("Authorization", "Bearer secret")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "media_converter_queue_depth") {
		t.Fatalf("unexpected metrics response: %d %s", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/metrics", nil)
	request.Header.Set("Authorization", "Bearer secret")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("metrics should be read-only, got %d", recorder.Code)
	}
}

func TestArtifactSupportsRange(t *testing.T) {
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	final, tmp, err := store.Begin(context.Background(), "artifact-range", ".mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(context.Background(), tmp, final); err != nil {
		t.Fatal(err)
	}
	states, _ := state.NewStore(t.TempDir())
	cfg := config.Config{BearerToken: "secret", QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/artifacts/artifact-range", nil)
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Range", "bytes=2-5")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status %d", recorder.Code)
	}
	if recorder.Body.String() != "2345" {
		t.Fatalf("body %q", recorder.Body.String())
	}
}

func TestArtifactEndpointReadsLegacyRoot(t *testing.T) {
	input, output, legacy := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(legacy+"/legacy-artifact.jpg", []byte("legacy bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewLocalStoreWithModeAndLegacy(input, output, storage.OutputModeLocal, legacy)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BearerToken: "secret", QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/artifacts/legacy-artifact", nil)
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "legacy bytes" {
		t.Fatalf("legacy artifact response %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestArtifactUploadStagesRawBytes(t *testing.T) {
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BearerToken: "secret", MaxInputBytes: 32, QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)
	server.SetReady(true)

	request := httptest.NewRequest(http.MethodPost, "/v1/artifacts", bytes.NewReader([]byte("audio bytes")))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "audio/wav")
	request.Header.Set("X-Filename", "sample.wav")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload status %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ArtifactID   string `json:"artifact_id"`
		SHA256       string `json:"sha256"`
		Size         int64  `json:"size"`
		DeclaredMIME string `json:"declared_mime"`
		Filename     string `json:"filename"`
		State        string `json:"state"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ArtifactID == "" || response.Size != 11 || response.DeclaredMIME != "audio/wav" || response.Filename != "sample.wav" || response.State != "staged" {
		t.Fatalf("unexpected upload response: %+v", response)
	}
	if _, err := store.OpenInput(context.Background(), response.ArtifactID); err != nil {
		t.Fatal(err)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/artifacts", bytes.NewReader([]byte("x")))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "audio/wav")
	request.Header.Set("X-Filename", "../escape.wav")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unsafe filename status %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/artifacts", bytes.NewReader(bytes.Repeat([]byte("x"), 33)))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "audio/wav")
	request.Header.Set("X-Filename", "large.wav")
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload status %d", recorder.Code)
	}
}

func TestAudioUploadSubmitDownloadEndToEnd(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is unavailable")
	}
	root := t.TempDir()
	fixture := root + "/voice.wav"
	cmd := exec.Command(ffmpeg, "-y", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000", "-t", "0.2", "-ac", "1", "-c:a", "pcm_s16le", "-ar", "16000", fixture)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create audio fixture: %v: %s", err, output)
	}
	bytesInput, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BearerToken: "secret", FFmpegPath: ffmpeg, FFprobePath: ffprobe, MaxInputBytes: 10 << 20, MaxOutputBytes: 10 << 20, QueueSize: 2, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, JobTimeout: 20 * time.Second, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)
	server.SetReady(true)

	upload := httptest.NewRequest(http.MethodPost, "/v1/artifacts", bytes.NewReader(bytesInput))
	upload.Header.Set("Authorization", "Bearer secret")
	upload.Header.Set("Content-Type", "audio/wav")
	upload.Header.Set("X-Filename", "voice.wav")
	uploadRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(uploadRecorder, upload)
	if uploadRecorder.Code != http.StatusCreated {
		t.Fatalf("upload status %d: %s", uploadRecorder.Code, uploadRecorder.Body.String())
	}
	var uploaded struct {
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal(uploadRecorder.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}

	policy := domain.DefaultPolicy()
	policy.TargetAudio = "voice_ogg_opus"
	policy.IncludeDownloadURLs = true
	jobBody, err := json.Marshal(domain.JobRequest{JobID: "A47", Items: []domain.Item{{ID: "voice", ArtifactID: uploaded.ArtifactID, DeclaredKind: "audio"}}, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	submit := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(jobBody))
	submit.Header.Set("Authorization", "Bearer secret")
	submitRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(submitRecorder, submit)
	if submitRecorder.Code != http.StatusAccepted {
		t.Fatalf("submit status %d: %s", submitRecorder.Code, submitRecorder.Body.String())
	}
	retry := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(jobBody))
	retry.Header.Set("Authorization", "Bearer secret")
	retryRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(retryRecorder, retry)
	if retryRecorder.Code != http.StatusOK {
		t.Fatalf("idempotent retry status %d: %s", retryRecorder.Code, retryRecorder.Body.String())
	}
	secondUpload := httptest.NewRequest(http.MethodPost, "/v1/artifacts", bytes.NewReader(bytesInput))
	secondUpload.Header.Set("Authorization", "Bearer secret")
	secondUpload.Header.Set("Content-Type", "audio/wav")
	secondUpload.Header.Set("X-Filename", "voice-second.wav")
	secondUploadRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(secondUploadRecorder, secondUpload)
	if secondUploadRecorder.Code != http.StatusCreated {
		t.Fatalf("second upload status %d: %s", secondUploadRecorder.Code, secondUploadRecorder.Body.String())
	}
	var secondUploaded struct {
		ArtifactID string `json:"artifact_id"`
	}
	if err := json.Unmarshal(secondUploadRecorder.Body.Bytes(), &secondUploaded); err != nil {
		t.Fatal(err)
	}
	conflicting := domain.JobRequest{JobID: "A47", Items: []domain.Item{{ID: "voice", ArtifactID: secondUploaded.ArtifactID, DeclaredKind: "audio"}}, Policy: policy}
	conflictingBody, err := json.Marshal(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	conflictRequest := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(conflictingBody))
	conflictRequest.Header.Set("Authorization", "Bearer secret")
	conflictRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(conflictRecorder, conflictRequest)
	if conflictRecorder.Code != http.StatusConflict {
		t.Fatalf("conflicting submit status %d: %s", conflictRecorder.Code, conflictRecorder.Body.String())
	}

	var completed map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		poll := httptest.NewRequest(http.MethodGet, "/v1/jobs/A47", nil)
		poll.Header.Set("Authorization", "Bearer secret")
		pollRecorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(pollRecorder, poll)
		if pollRecorder.Code != http.StatusOK {
			t.Fatalf("poll status %d: %s", pollRecorder.Code, pollRecorder.Body.String())
		}
		if err := json.Unmarshal(pollRecorder.Body.Bytes(), &completed); err != nil {
			t.Fatal(err)
		}
		if completed["state"] == string(domain.JobCompleted) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if completed["state"] != string(domain.JobCompleted) {
		t.Fatalf("job did not complete: %#v", completed)
	}
	result := completed["result"].(map[string]any)
	items := result["items"].([]any)
	item := items[0].(map[string]any)
	if item["status"] != string(domain.ItemSuccess) {
		t.Fatalf("item failed: %#v", item)
	}
	outputMetadata := item["output"].(map[string]any)
	artifactID := outputMetadata["artifact_id"].(string)
	wantHash := outputMetadata["sha256"].(string)

	downloadsRequest := httptest.NewRequest(http.MethodGet, "/v1/jobs/A47/downloads", nil)
	downloadsRequest.Header.Set("Authorization", "Bearer secret")
	downloadsRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(downloadsRecorder, downloadsRequest)
	if downloadsRecorder.Code != http.StatusOK || !strings.Contains(downloadsRecorder.Body.String(), "/v1/artifacts/"+artifactID) {
		t.Fatalf("downloads response %d: %s", downloadsRecorder.Code, downloadsRecorder.Body.String())
	}

	download := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+artifactID, nil)
	download.Header.Set("Authorization", "Bearer secret")
	downloadRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(downloadRecorder, download)
	if downloadRecorder.Code != http.StatusOK || downloadRecorder.Header().Get("Content-Type") != "audio/ogg" {
		t.Fatalf("download response %d %q", downloadRecorder.Code, downloadRecorder.Header().Get("Content-Type"))
	}
	hash := sha256.Sum256(downloadRecorder.Body.Bytes())
	if hex.EncodeToString(hash[:]) != wantHash {
		t.Fatalf("download hash mismatch: got %s want %s", hex.EncodeToString(hash[:]), wantHash)
	}
}

func TestDiscoveryManifestIsStableContract(t *testing.T) {
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BearerToken: "discovery-secret", ProcessorVersion: "2.4.1", PolicyVersion: "media-v1.1", InputRoot: input, OutputRoot: output, QueueSize: 1, JobWorkers: 1, ImageWorkers: 2, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/.well-known/media-converter.json", nil))
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unexpected response: %d %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	if recorder.Header().Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("cache control %q", recorder.Header().Get("Cache-Control"))
	}
	var manifest map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["service"] != "media-converter" || manifest["version"] != "2.4.1" || manifest["api_version"] != "v1" {
		t.Fatalf("unexpected identity: %#v", manifest)
	}
	authentication := manifest["authentication"].(map[string]any)
	if authentication["type"] != "bearer" {
		t.Fatalf("unexpected authentication: %#v", authentication)
	}
	jobModel := manifest["job_model"].(map[string]any)
	if jobModel["async"] != true || !containsString(jobModel["states"], string(domain.JobQueued)) || !containsString(jobModel["states"], string(domain.JobCompleted)) {
		t.Fatalf("unexpected job model: %#v", jobModel)
	}
	endpoints := manifest["endpoints"].(map[string]any)
	for key, want := range map[string]string{"upload_artifact": "/v1/artifacts", "create_job": "/v1/jobs", "get_job": "/v1/jobs/{job_id}", "get_downloads": "/v1/jobs/{job_id}/downloads", "get_artifact": "/v1/artifacts/{artifact_id}", "capabilities": "/v1/capabilities", "health_ready": "/health/ready", "openapi": "/openapi.json"} {
		if endpoints[key] != want {
			t.Fatalf("endpoint %s = %v, want %s", key, endpoints[key], want)
		}
	}
	canonical := manifest["media_contract"].(map[string]any)["canonical_output"].(map[string]any)
	image := canonical["image"].(map[string]any)
	video := canonical["video"].(map[string]any)
	if image["mime"] != "image/jpeg" || image["extension"] != ".jpg" || video["mime"] != "video/mp4" || video["video_codec"] != "h264" || video["pixel_format"] != "yuv420p" {
		t.Fatalf("unexpected canonical output: %#v", canonical)
	}
	audio := canonical["audio"].([]any)
	if len(audio) != 3 {
		t.Fatalf("unexpected audio policies: %#v", audio)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "discovery-secret") || strings.Contains(body, input) || strings.Contains(body, output) {
		t.Fatalf("manifest exposes secret or filesystem path: %s", body)
	}
}

func TestCapabilitiesReflectRuntimeConfiguration(t *testing.T) {
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BearerToken: "capability-secret", ProcessorVersion: "2.4.1", PolicyVersion: "media-v1.1", ToolAvailability: map[string]bool{"ffmpeg": true, "ffprobe": true, "imagemagick": true}, ImageFormats: map[string]bool{"jpeg": true, "png": true, "webp": true, "heic": false, "heif": false, "avif": true}, ImageWorkers: 4, VideoWorkers: 1, JobWorkers: 2, QueueSize: 7, MaxInputBytes: 1234, MaxOutputBytes: 5678, MaxDuration: 90 * time.Second, MaxWidth: 1920, MaxHeight: 1080, MaxPixels: 2073600, MaxItemsPerJob: 9, MaxConcurrentItemsPerJob: 2, InputRoot: input, OutputRoot: output, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
	var capabilities map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	runtime := capabilities["runtime"].(map[string]any)
	formats := capabilities["formats"].(map[string]any)
	workers := capabilities["workers"].(map[string]any)
	limits := capabilities["limits"].(map[string]any)
	features := capabilities["features"].(map[string]any)
	if runtime["ffmpeg"] != true || runtime["ffprobe"] != true || runtime["imagemagick"] != true || formats["heic"] != false || formats["avif"] != true {
		t.Fatalf("unexpected runtime/formats: %#v %#v", runtime, formats)
	}
	if workers["image_concurrency"] != float64(4) || workers["video_concurrency"] != float64(1) || workers["queue_capacity"] != float64(7) {
		t.Fatalf("unexpected workers: %#v", workers)
	}
	if limits["max_input_bytes"] != float64(1234) || limits["max_video_duration_seconds"] != float64(90) || limits["max_items_per_job"] != float64(9) {
		t.Fatalf("unexpected limits: %#v", limits)
	}
	if features["artifact_upload"] != true || features["staging"] != true || features["download_urls"] != true || features["download_url_mode"] != "relative" {
		t.Fatalf("unexpected features: %#v", features)
	}
	if policies, ok := capabilities["audio_policies"].([]any); !ok || len(policies) != 3 {
		t.Fatalf("unexpected audio capabilities: %#v", capabilities["audio_policies"])
	}
	body := recorder.Body.String()
	if strings.Contains(body, "capability-secret") || strings.Contains(body, input) || strings.Contains(body, output) {
		t.Fatalf("capabilities expose secret or filesystem path: %s", body)
	}
}

func TestOpenAPIContract(t *testing.T) {
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BearerToken: "openapi-secret", QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unexpected response: %d %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	var document map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	for _, path := range []string{"/.well-known/media-converter.json", "/v1/capabilities", "/v1/artifacts", "/v1/jobs", "/v1/jobs/{job_id}", "/v1/artifacts/{artifact_id}", "/health/live", "/health/ready", "/metrics"} {
		if _, ok := paths[path]; !ok {
			t.Fatalf("OpenAPI path %q is missing", path)
		}
	}
	components := document["components"].(map[string]any)
	bearer := components["securitySchemes"].(map[string]any)["bearerAuth"].(map[string]any)
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("unexpected bearer scheme: %#v", bearer)
	}
	post := paths["/v1/jobs"].(map[string]any)["post"].(map[string]any)
	if _, ok := post["responses"].(map[string]any)["202"]; !ok {
		t.Fatal("POST /v1/jobs does not document 202")
	}
	schemas := components["schemas"].(map[string]any)
	stateProperty := schemas["JobResponse"].(map[string]any)["properties"].(map[string]any)["state"].(map[string]any)
	enumStates := stateProperty["enum"].([]any)
	if !containsString(enumStates, string(domain.JobQueued)) || !containsString(enumStates, string(domain.JobProcessing)) || !containsString(enumStates, string(domain.JobCompleted)) {
		t.Fatalf("job state schema is incomplete: %#v", enumStates)
	}
	if _, ok := paths["/v1/artifacts"].(map[string]any)["post"]; !ok {
		t.Fatal("POST /v1/artifacts is missing")
	}
	if _, ok := paths["/v1/jobs/{job_id}/downloads"].(map[string]any)["get"]; !ok {
		t.Fatal("GET /v1/jobs/{job_id}/downloads is missing")
	}
}

func TestJobDownloadsEndpoint(t *testing.T) {
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("converted media")
	final, tmp, err := store.Begin(context.Background(), "out-download", ".mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		t.Fatal(err)
	}
	committed, err := store.Commit(context.Background(), tmp, final)
	if err != nil {
		t.Fatal(err)
	}

	policy := domain.DefaultPolicy()
	policy.IncludeDownloadURLs = true
	record := domain.JobRecord{
		JobID:     "download-job",
		Request:   domain.JobRequest{JobID: "download-job", Policy: policy},
		State:     domain.JobCompleted,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Result: &domain.JobResult{Outcome: domain.OutcomeSuccess, Items: []domain.ItemResult{{
			ID:     "item-1",
			Status: domain.ItemSuccess,
			Output: &domain.OutputMetadata{ArtifactID: committed.ID, Filename: "converted.mp4", MIME: "video/mp4", Size: int64(len(content))},
		}}},
	}
	if err := states.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{BearerToken: "secret", PublicBaseURL: "https://media.example.test", QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)

	request := httptest.NewRequest(http.MethodGet, "/v1/jobs/download-job/downloads", nil)
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("downloads status %d: %s", recorder.Code, recorder.Body.String())
	}
	var response downloadsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Downloads) != 1 || response.Downloads[0].Output == nil {
		t.Fatalf("unexpected downloads response: %#v", response)
	}
	wantURL := "https://media.example.test/v1/artifacts/" + committed.ID
	if got := response.Downloads[0].Output.DownloadURL; got != wantURL {
		t.Fatalf("download URL = %q, want %q", got, wantURL)
	}

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/jobs/download-job/downloads", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status %d", unauthorized.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+committed.ID, nil)
	request.Header.Set("Authorization", "Bearer secret")
	artifactRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(artifactRecorder, request)
	if artifactRecorder.Code != http.StatusOK || artifactRecorder.Body.String() != string(content) {
		t.Fatalf("artifact download status %d body %q", artifactRecorder.Code, artifactRecorder.Body.String())
	}
}

func TestJobDownloadsRequiresOptInAndReportsPending(t *testing.T) {
	store, err := storage.NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{BearerToken: "secret", QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)

	for _, test := range []struct {
		name   string
		policy domain.Policy
		state  domain.JobState
		status int
	}{
		{name: "opt in required", policy: domain.DefaultPolicy(), state: domain.JobCompleted, status: http.StatusBadRequest},
		{name: "pending job", policy: func() domain.Policy { p := domain.DefaultPolicy(); p.IncludeDownloadURLs = true; return p }(), state: domain.JobProcessing, status: http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			jobID := "download-" + strings.ReplaceAll(test.name, " ", "-")
			record := domain.JobRecord{JobID: jobID, Request: domain.JobRequest{JobID: jobID, Policy: test.policy}, State: test.state}
			if err := states.Put(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+jobID+"/downloads", nil)
			request.Header.Set("Authorization", "Bearer secret")
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestWebDAVDownloadURLUsesConfiguredBase(t *testing.T) {
	server := &Server{cfg: config.Config{
		OutputMode:    config.OutputModeWebDAV,
		PublicBaseURL: "https://legacy.example.test",
		WebDAVBaseURL: "https://files.example.test/media-converter-v2/artifacts",
	}}
	got := server.artifactURL("norm-abc123")
	want := "https://files.example.test/media-converter-v2/artifacts/norm-abc123"
	if got != want {
		t.Fatalf("WebDAV download URL = %q, want %q", got, want)
	}
	if strings.Contains(got, "legacy.example.test") {
		t.Fatal("WebDAV download URL fell back to the converter URL")
	}
	got = server.artifactURLForFile("norm-abc123", "converted clip.mp4")
	want = "https://files.example.test/media-converter-v2/artifacts/converted%20clip.mp4"
	if got != want {
		t.Fatalf("WebDAV filename URL = %q, want %q", got, want)
	}
}

func TestWebDAVReadinessFailsClosedWhenMountIsUnavailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mount readiness is enforced for the Linux VPS target")
	}
	input, output := t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStoreWithMode(input, output, storage.OutputModeWebDAV)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		BearerToken:   "secret",
		OutputMode:    config.OutputModeWebDAV,
		WebDAVBaseURL: "https://files.example.test/media-converter-v2/artifacts",
		QueueSize:     1,
		JobWorkers:    1,
		ImageWorkers:  1,
		VideoWorkers:  1,
		WorkRoot:      t.TempDir(),
	}
	manager := job.NewManager(cfg, store, states)
	defer manager.Stop()
	server := NewServer(cfg, manager, store, nil)
	server.SetReadiness(true, map[string]bool{"artifact_storage": true, "workspace": true})

	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"artifact_storage":false`) {
		t.Fatalf("readiness did not report the missing mount: %s", recorder.Body.String())
	}
}

func TestWebDAVReadinessDoesNotBlockOnHungProbe(t *testing.T) {
	server := &Server{
		cfg:    config.Config{OutputMode: config.OutputModeWebDAV},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	server.outputReady = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	server.SetReadiness(true, map[string]bool{"artifact_storage": true, "workspace": true})

	started := time.Now()
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if elapsed := time.Since(started); elapsed > readinessProbeTimeout+500*time.Millisecond {
		t.Fatalf("readiness probe blocked for %s", elapsed)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("readiness status %d: %s", recorder.Code, recorder.Body.String())
	}

	deadline := time.Now().Add(readinessProbeTimeout + 500*time.Millisecond)
	for time.Now().Before(deadline) && server.ready.Load() {
		time.Sleep(10 * time.Millisecond)
	}
	if server.ready.Load() {
		t.Fatal("hung readiness probe did not fail the cached readiness state")
	}
}

func containsString(values any, want string) bool {
	items, ok := values.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
