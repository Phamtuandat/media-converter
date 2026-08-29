package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"media-converter-v2/internal/config"
	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/job"
	"media-converter-v2/internal/observability"
	"media-converter-v2/internal/state"
	"media-converter-v2/internal/storage"
)

type Server struct {
	cfg                config.Config
	manager            *job.Manager
	store              *storage.LocalStore
	ready              atomic.Bool
	staticReady        atomic.Bool
	logger             *slog.Logger
	metrics            *observability.Metrics
	readyChecks        atomic.Value
	readinessMu        sync.Mutex
	readinessInFlight  bool
	lastReadinessStart time.Time
	outputReady        func(context.Context) error
}

const (
	readinessProbeInterval      = 30 * time.Second
	readinessProbeRetryInterval = 5 * time.Second
	readinessProbeTimeout       = 2 * time.Second
)

func NewServer(cfg config.Config, manager *job.Manager, store *storage.LocalStore, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{
		cfg: cfg, manager: manager, store: store, logger: logger, metrics: manager.Metrics(),
		// Health checks must not perform a write/delete cycle through WebDAV.
		// Mount identity is sufficient to fail closed without blocking on FUSE.
		outputReady: store.CheckOutputMounted,
	}
	server.readyChecks.Store(map[string]bool{})
	return server
}

func (s *Server) SetReady(value bool) {
	s.staticReady.Store(value)
	s.ready.Store(value)
}

func (s *Server) SetReadiness(value bool, checks map[string]bool) {
	s.readyChecks.Store(copyChecks(checks))
	staticReady := true
	for name, check := range checks {
		if name != "artifact_storage" && !check {
			staticReady = false
		}
	}
	s.staticReady.Store(staticReady)
	s.ready.Store(value)
}

// RefreshReadiness lets the supervisor re-arm a service after a WebDAV mount
// is restored without restarting the process.
func (s *Server) RefreshReadiness(ctx context.Context) bool {
	return s.refreshReadiness(ctx)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/media-converter.json", s.discovery)
	mux.HandleFunc("/openapi.json", s.openapi)
	mux.HandleFunc("/health/live", s.live)
	mux.HandleFunc("/health/ready", s.readyHandler)
	mux.HandleFunc("/v1/capabilities", s.capabilities)
	mux.Handle("/metrics", s.auth(http.HandlerFunc(s.metricsHandler)))
	mux.Handle("/v1/jobs", s.auth(http.HandlerFunc(s.jobs)))
	mux.Handle("/v1/jobs/{job_id}/downloads", s.auth(http.HandlerFunc(s.downloads)))
	mux.Handle("/v1/jobs/", s.auth(http.HandlerFunc(s.jobByID)))
	mux.Handle("/v1/artifacts", s.auth(http.HandlerFunc(s.artifactUpload)))
	mux.Handle("/v1/artifacts/", s.auth(http.HandlerFunc(s.artifact)))
	return logging(mux, s.logger)
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}
func (s *Server) readyHandler(w http.ResponseWriter, r *http.Request) {
	// Never make a health request wait on WebDAV/FUSE. The cached result is
	// updated by the bounded background probe below.
	s.scheduleReadinessProbe()
	checks, _ := s.readyChecks.Load().(map[string]bool)
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "checks": checks})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "checks": checks})
}

func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, s.metrics.Prometheus())
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.scheduleReadinessProbe()
	if !s.ready.Load() {
		writeError(w, http.StatusServiceUnavailable, domain.NewError("converter_unavailable", "service is not ready", "startup", true, nil))
		return
	}
	var request domain.JobRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, domain.NewError(domain.CodeRequestInvalid, "invalid JSON request", "request", false, err))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, domain.NewError(domain.CodeRequestInvalid, "request body must contain one JSON object", "request", false, nil))
		return
	}
	request = request.Normalized()
	record, existing, err := s.manager.Submit(r.Context(), request)
	if err != nil {
		if de, ok := err.(*domain.Error); ok && de.Code == "queue_full" {
			writeError(w, http.StatusTooManyRequests, de)
			return
		}
		if de, ok := err.(*domain.Error); ok && de.Code == domain.CodeConflict {
			writeError(w, http.StatusConflict, de)
			return
		}
		if de, ok := err.(*domain.Error); ok && de.Code == domain.CodeRequestInvalid {
			writeError(w, http.StatusBadRequest, de)
			return
		}
		writeError(w, http.StatusServiceUnavailable, domain.NewError("storage_read_failed", "job could not be accepted", "request", true, err))
		return
	}
	if existing {
		writeJSON(w, http.StatusOK, jobResponse(record))
		return
	}
	w.Header().Set("Location", "/v1/jobs/"+record.JobID)
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": record.JobID, "state": string(record.State)})
}

func (s *Server) jobByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := strings.TrimPrefix(r.URL.Path, "/v1/jobs/")
	if jobID == "" || filepath.Base(jobID) != jobID {
		http.NotFound(w, r)
		return
	}
	record, err := s.manager.Get(r.Context(), jobID)
	if errors.Is(err, state.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, jobResponse(record))
}

type artifactUploadResponse struct {
	ArtifactID   string `json:"artifact_id"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	DeclaredMIME string `json:"declared_mime"`
	Filename     string `json:"filename"`
	State        string `json:"state"`
}

func (s *Server) artifactUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.scheduleReadinessProbe()
	if !s.ready.Load() {
		writeError(w, http.StatusServiceUnavailable, domain.NewError("converter_unavailable", "service is not ready", "startup", true, nil))
		return
	}
	declaredMIME := strings.TrimSpace(r.Header.Get("Content-Type"))
	filename := strings.TrimSpace(r.Header.Get("X-Filename"))
	if declaredMIME == "" || filename == "" || len(filename) > 255 || filename == "." || filename == ".." || filepath.Base(filename) != filename || strings.ContainsAny(filename, "/\\\x00") {
		writeError(w, http.StatusBadRequest, domain.NewError(domain.CodeRequestInvalid, "Content-Type and X-Filename are required and must be safe", "upload", false, nil))
		return
	}
	limit := s.cfg.MaxInputBytes
	if limit <= 0 {
		limit = 512 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+1)
	artifact, err := s.store.Stage(r.Context(), r.Body, limit)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, domain.NewError("input_size_exceeded", "artifact exceeds configured size limit", "upload", false, err))
			return
		}
		if de, ok := err.(*domain.Error); ok {
			switch de.Code {
			case domain.CodeRequestInvalid, "invalid_artifact_id":
				writeError(w, http.StatusBadRequest, de)
				return
			case "input_size_exceeded":
				writeError(w, http.StatusRequestEntityTooLarge, de)
				return
			}
		}
		writeError(w, http.StatusServiceUnavailable, domain.NewError("storage_write_failed", "artifact could not be staged", "upload", true, err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, artifactUploadResponse{
		ArtifactID: artifact.ID, SHA256: artifact.SHA256, Size: artifact.Size,
		DeclaredMIME: declaredMIME, Filename: filename, State: "staged",
	})
}

func (s *Server) refreshReadiness(ctx context.Context) bool {
	if s.cfg.OutputMode != config.OutputModeWebDAV {
		return s.ready.Load()
	}

	if ctx == nil {
		ctx = context.Background()
	}
	s.readinessMu.Lock()
	now := time.Now()
	interval := readinessProbeInterval
	if !s.ready.Load() {
		interval = readinessProbeRetryInterval
	}
	if s.readinessInFlight || (!s.lastReadinessStart.IsZero() && now.Sub(s.lastReadinessStart) < interval) {
		ready := s.ready.Load()
		s.readinessMu.Unlock()
		return ready
	}
	s.readinessInFlight = true
	s.lastReadinessStart = now
	probe := s.outputReady
	s.readinessMu.Unlock()

	if probe == nil {
		s.finishReadinessProbe(false)
		return false
	}

	done := make(chan bool, 1)
	go func() {
		probeCtx, cancel := context.WithTimeout(ctx, readinessProbeTimeout)
		defer cancel()
		ready := probe(probeCtx) == nil
		s.finishReadinessProbe(ready)
		done <- ready
	}()

	timer := time.NewTimer(readinessProbeTimeout)
	defer timer.Stop()
	select {
	case ready := <-done:
		return ready && s.staticReady.Load()
	case <-timer.C:
		s.setArtifactReadiness(false)
		s.logger.Warn("readiness_probe_timeout", "timeout", readinessProbeTimeout.String())
		return false
	case <-ctx.Done():
		s.setArtifactReadiness(false)
		return false
	}
}

// scheduleReadinessProbe starts at most one bounded WebDAV writability probe
// without putting the probe's latency on an HTTP request path.
func (s *Server) scheduleReadinessProbe() {
	if s.cfg.OutputMode != config.OutputModeWebDAV {
		return
	}

	s.readinessMu.Lock()
	now := time.Now()
	interval := readinessProbeInterval
	if !s.ready.Load() {
		interval = readinessProbeRetryInterval
	}
	if s.readinessInFlight || (!s.lastReadinessStart.IsZero() && now.Sub(s.lastReadinessStart) < interval) {
		s.readinessMu.Unlock()
		return
	}
	s.readinessInFlight = true
	s.lastReadinessStart = now
	probe := s.outputReady
	s.readinessMu.Unlock()

	go func() {
		if probe == nil {
			s.finishReadinessProbe(false)
			return
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), readinessProbeTimeout)
		defer cancel()
		probeDone := make(chan bool, 1)
		go func() {
			probeDone <- probe(probeCtx) == nil
		}()
		timer := time.NewTimer(readinessProbeTimeout)
		defer timer.Stop()
		select {
		case ready := <-probeDone:
			s.finishReadinessProbe(ready)
		case <-timer.C:
			s.logger.Warn("readiness_probe_timeout", "timeout", readinessProbeTimeout.String())
			s.finishReadinessProbe(false)
		}
	}()
}

func (s *Server) finishReadinessProbe(outputReady bool) {
	s.setArtifactReadiness(outputReady)
	s.readinessMu.Lock()
	s.readinessInFlight = false
	s.readinessMu.Unlock()
}

func (s *Server) setArtifactReadiness(outputReady bool) {
	checks, _ := s.readyChecks.Load().(map[string]bool)
	updated := copyChecks(checks)
	updated["artifact_storage"] = outputReady
	s.readyChecks.Store(updated)
	s.ready.Store(s.staticReady.Load() && outputReady)
}

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/artifacts/")
	if id == "" || filepath.Base(id) != id {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodDelete {
		var err error
		if strings.HasPrefix(id, "stg-") {
			err = s.store.RemoveInput(r.Context(), id)
		} else {
			err = s.store.Remove(r.Context(), id)
		}
		if err != nil {
			if de, ok := err.(*domain.Error); ok && de.Code == "invalid_artifact_id" {
				writeError(w, http.StatusBadRequest, de)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	artifact, err := s.store.OpenCommitted(r.Context(), id)
	if err != nil {
		if de, ok := err.(*domain.Error); ok {
			if de.Code == "invalid_artifact_id" {
				writeError(w, http.StatusBadRequest, de)
				return
			}
			if de.Code == "storage_not_mounted" {
				writeError(w, http.StatusServiceUnavailable, de)
				return
			}
			if de.Code == "artifact_not_found" || de.Code == "input_missing" {
				http.NotFound(w, r)
				return
			}
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if artifact.File == nil {
		writeError(w, http.StatusInternalServerError, domain.NewError("storage_read_failed", "could not open artifact", "artifact", true, nil))
		return
	}
	defer artifact.File.Close()
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(artifact.Path)))
	contentType := "application/octet-stream"
	if strings.HasSuffix(artifact.Path, ".jpg") {
		contentType = "image/jpeg"
	}
	if strings.HasSuffix(artifact.Path, ".mp4") {
		contentType = "video/mp4"
	}
	if strings.HasSuffix(artifact.Path, ".ogg") {
		contentType = "audio/ogg"
	}
	if strings.HasSuffix(artifact.Path, ".wav") {
		contentType = "audio/wav"
	}
	if strings.HasSuffix(artifact.Path, ".m4a") {
		contentType = "audio/mp4"
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, filepath.Base(artifact.Path), time.Time{}, artifact.File)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := "Bearer " + s.cfg.BearerToken
		provided := r.Header.Get("Authorization")
		if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="media-converter"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func jobResponse(record domain.JobRecord) map[string]any {
	response := map[string]any{"job_id": record.JobID, "state": record.State}
	if record.State == domain.JobCompleted {
		response["result"] = record.Result
		response["processor"] = record.Processor
	}
	return response
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, err error) {
	var de *domain.Error
	if errors.As(err, &de) {
		writeJSON(w, status, map[string]any{"error": domain.ProcessingError{Code: de.Code, Message: de.Message, Retryable: de.Retryable, Stage: de.Stage}})
		return
	}
	writeJSON(w, status, map[string]any{"error": domain.ProcessingError{Code: "internal_error", Message: "internal server error", Retryable: false}})
}
func logging(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		logger.Info("http_request", "method", r.Method, "path", r.URL.Path, "status", rw.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func copyChecks(checks map[string]bool) map[string]bool {
	copy := make(map[string]bool, len(checks))
	for key, value := range checks {
		copy[key] = value
	}
	return copy
}
