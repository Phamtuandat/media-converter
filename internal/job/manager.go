package job

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"media-converter-v2/internal/cache"
	"media-converter-v2/internal/config"
	"media-converter-v2/internal/detection"
	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/mediaaudio"
	"media-converter-v2/internal/mediaimage"
	"media-converter-v2/internal/mediavideo"
	"media-converter-v2/internal/observability"
	"media-converter-v2/internal/processor"
	"media-converter-v2/internal/state"
	"media-converter-v2/internal/storage"
)

type Manager struct {
	cfg       config.Config
	store     *storage.LocalStore
	states    StateStore
	cache     *cache.Store
	detector  detection.Detector
	audio     mediaaudio.Pipeline
	images    mediaimage.Pipeline
	videos    mediavideo.Pipeline
	queue     chan string
	imageSem  chan struct{}
	videoSem  chan struct{}
	mu        sync.Mutex
	wg        sync.WaitGroup
	startOnce sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	metrics   *observability.Metrics
	logger    *slog.Logger
	queueAt   sync.Map
}

type StateStore interface {
	Get(context.Context, string) (domain.JobRecord, error)
	Put(context.Context, domain.JobRecord) error
	Delete(context.Context, string) error
	List(context.Context) ([]domain.JobRecord, error)
	CleanupOld(context.Context, time.Duration) error
}

type progressTracker struct {
	manager *Manager
	mu      sync.Mutex
	record  domain.JobRecord
}

func newProgressTracker(manager *Manager, record domain.JobRecord) *progressTracker {
	return &progressTracker{manager: manager, record: record}
}

func (p *progressTracker) update(ctx context.Context, index int, update func(*domain.ItemProgress)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.record.Progress) {
		return fmt.Errorf("invalid progress index")
	}
	update(&p.record.Progress[index])
	p.record.UpdatedAt = time.Now().UTC()
	snapshot := p.record
	snapshot.Progress = append([]domain.ItemProgress(nil), p.record.Progress...)
	if err := p.manager.persistState(ctx, snapshot); err != nil {
		p.manager.logger.Error("job_progress_persist_failed", "job_id", snapshot.JobID, "item_id", snapshot.Progress[index].ID, "error", err)
		p.manager.metrics.RecordError("state_write_failed")
		return err
	}
	return nil
}

func (p *progressTracker) snapshot() domain.JobRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.record
}

func (p *progressTracker) hasCommittedResult(index int, result domain.ItemResult) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.record.Progress) || p.record.Progress[index].State != domain.ProgressCommitted || p.record.Progress[index].Result == nil {
		return false
	}
	return reflect.DeepEqual(*p.record.Progress[index].Result, result)
}

func NewManager(cfg config.Config, store *storage.LocalStore, states StateStore, caches ...*cache.Store) *Manager {
	if cfg.MaxItemsPerJob < 1 {
		cfg.MaxItemsPerJob = 64
	}
	if cfg.MaxConcurrentItemsPerJob < 1 {
		cfg.MaxConcurrentItemsPerJob = 1
	}
	if cfg.MaxAggregateInputBytes <= 0 {
		cfg.MaxAggregateInputBytes = 2 << 30
	}
	if cfg.MaxAggregatePixels <= 0 {
		cfg.MaxAggregatePixels = 8 * 10000 * 10000
	}
	if cfg.MaxAggregateDuration <= 0 {
		cfg.MaxAggregateDuration = 2 * time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	var resultCache *cache.Store
	if len(caches) > 0 {
		resultCache = caches[0]
	}
	m := &Manager{
		cfg: cfg, store: store, states: states, cache: resultCache, queue: make(chan string, cfg.QueueSize),
		imageSem: make(chan struct{}, cfg.ImageWorkers), videoSem: make(chan struct{}, cfg.VideoWorkers),
		detector: detection.Detector{Magick: processor.ToolRunner{Path: cfg.MagickPath}},
		audio:    mediaaudio.Pipeline{FFmpeg: processor.ToolRunner{Path: cfg.FFmpegPath}, Probe: mediavideo.FFProbe{Runner: processor.ToolRunner{Path: cfg.FFprobePath}}, MaxDurationMS: cfg.MaxDuration.Milliseconds(), MaxOutputBytes: cfg.MaxOutputBytes, FFmpegThreads: cfg.FFmpegThreads},
		images:   mediaimage.Pipeline{Magick: processor.ToolRunner{Path: cfg.MagickPath}, MaxWidth: cfg.MaxWidth, MaxHeight: cfg.MaxHeight, MaxPixels: cfg.MaxPixels, MaxOutputBytes: cfg.MaxOutputBytes},
		videos:   mediavideo.Pipeline{FFmpeg: processor.ToolRunner{Path: cfg.FFmpegPath}, Probe: mediavideo.FFProbe{Runner: processor.ToolRunner{Path: cfg.FFprobePath}}, Magick: processor.ToolRunner{Path: cfg.MagickPath}, MaxWidth: cfg.MaxWidth, MaxHeight: cfg.MaxHeight, MaxDurationMS: cfg.MaxDuration.Milliseconds(), MaxOutputBytes: cfg.MaxOutputBytes, FFmpegThreads: cfg.FFmpegThreads},
		ctx:      ctx, cancel: cancel,
		metrics: observability.NewMetrics(), logger: slog.Default(),
	}
	return m
}

func (m *Manager) Start() {
	m.startOnce.Do(func() {
		for i := 0; i < m.cfg.JobWorkers; i++ {
			m.wg.Add(1)
			go m.worker()
		}
	})
}

func (m *Manager) Recover(ctx context.Context) error {
	stateStarted := time.Now()
	records, err := m.states.List(ctx)
	m.recordPhase(observability.PhaseStateIO, stateStarted)
	if err != nil {
		m.metrics.RecordRecovery(err)
		return err
	}
	// Workers must consume recovered jobs while they are being enqueued; otherwise
	// recovery can block forever when persisted jobs exceed queue capacity.
	m.Start()
	for index := range records {
		record := &records[index]
		switch record.State {
		case domain.JobProcessing:
			result, reconcileErr := m.reconcileProgress(ctx, *record)
			if reconcileErr != nil {
				m.logger.Error("job_recovery_reconcile_failed", "job_id", record.JobID, "error", reconcileErr)
				m.metrics.RecordRecovery(reconcileErr)
				return reconcileErr
			}
			record.Result = &result
			record.State = domain.JobCompleted
			record.UpdatedAt = time.Now().UTC()
			m.logger.Info("job_recovered", "job_id", record.JobID, "outcome", result.Outcome)
			if err := m.persistState(ctx, *record); err != nil {
				m.logger.Error("job_recovery_persist_failed", "job_id", record.JobID, "error", err)
				m.metrics.RecordRecovery(err)
				return err
			}
		}
	}
	for _, record := range records {
		if record.State != domain.JobQueued {
			continue
		}
		m.queueAt.Store(record.JobID, time.Now())
		select {
		case m.queue <- record.JobID:
			m.metrics.SetQueueDepth(len(m.queue))
			m.logger.Info("job_recovered", "job_id", record.JobID, "state", record.State)
		case <-ctx.Done():
			m.queueAt.Delete(record.JobID)
			m.metrics.RecordRecovery(ctx.Err())
			return ctx.Err()
		}
	}
	m.metrics.RecordRecovery(nil)
	return nil
}

func (m *Manager) reconcileProgress(ctx context.Context, record domain.JobRecord) (domain.JobResult, error) {
	results := make([]domain.ItemResult, len(record.Request.Items))
	for index, item := range record.Request.Items {
		if index >= len(record.Progress) || record.Progress[index].ID != item.ID {
			results[index] = recoveryInterrupted(item.ID)
			continue
		}
		progress := record.Progress[index]
		if progress.State == domain.ProgressRejected || progress.State == domain.ProgressFailed {
			if progress.Result != nil {
				results[index] = *progress.Result
			} else {
				results[index] = recoveryInterrupted(item.ID)
			}
			continue
		}
		result, err := m.reconcileItemWithPolicy(ctx, progress, record.Request.Policy.TargetAudio)
		if err != nil {
			results[index] = domain.ItemResult{ID: item.ID, Status: domain.ItemFailed, Error: asProcessingError(domain.NewError("output_validation_failed", "committed artifact failed recovery validation", "recovery", false, err))}
			continue
		}
		results[index] = result
	}
	result := domain.JobResult{Items: results}
	result.Outcome = result.Aggregate()
	return result, nil
}

func recoveryInterrupted(id string) domain.ItemResult {
	return domain.ItemResult{ID: id, Status: domain.ItemFailed, Error: asProcessingError(domain.NewError("recovery_interrupted", "job interrupted before artifact commit", "recovery", true, nil))}
}

func (m *Manager) reconcileItem(ctx context.Context, progress domain.ItemProgress) (domain.ItemResult, error) {
	return m.reconcileItemWithPolicy(ctx, progress, "")
}

func (m *Manager) reconcileItemWithPolicy(ctx context.Context, progress domain.ItemProgress, audioPolicy string) (domain.ItemResult, error) {
	result := domain.ItemResult{ID: progress.ID, Status: domain.ItemSuccess, Operation: progress.Operation, Input: progress.Input, Detected: progress.Detected}
	if progress.Result != nil {
		result = *progress.Result
	}
	artifactID := progress.OutputArtifactID
	if artifactID == "" && result.Output != nil {
		artifactID = result.Output.ArtifactID
	}
	committedArtifact := artifactID != ""
	if artifactID == "" {
		artifactID = progress.PlannedOutputID
	}
	if artifactID == "" {
		return recoveryInterrupted(progress.ID), nil
	}
	if result.Output == nil {
		result.Output = &domain.OutputMetadata{ArtifactID: artifactID, Extension: extensionForKind(progress.Kind), MIME: mimeForKind(progress.Kind)}
	} else {
		result.Output.ArtifactID = artifactID
	}
	artifact, err := m.store.OpenCommitted(ctx, artifactID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "does not exist") {
			if committedArtifact {
				return domain.ItemResult{}, fmt.Errorf("committed artifact is missing")
			}
			return recoveryInterrupted(progress.ID), nil
		}
		return domain.ItemResult{}, err
	}
	defer artifact.File.Close()
	if result.Output.SHA256 != "" {
		if err := m.validateCommittedOutput(ctx, result.Output); err != nil {
			return domain.ItemResult{}, err
		}
	} else {
		var metadata domain.OutputMetadata
		if progress.Kind == "image" {
			metadata, err = m.validateImageOutput(ctx, artifact.Path)
		} else if progress.Kind == "audio" {
			metadata, err = m.validateAudioOutput(ctx, artifact.Path, audioPolicy)
		} else {
			metadata, err = m.validateVideoOutput(ctx, artifact.Path)
		}
		if err != nil {
			return domain.ItemResult{}, err
		}
		hash, size, hashErr := processor.HashReader(artifact.File)
		if hashErr != nil {
			return domain.ItemResult{}, hashErr
		}
		metadata.ArtifactID = artifactID
		metadata.Filename = filepath.Base(artifact.Path)
		metadata.SHA256 = hash
		metadata.Size = size
		result.Output = &metadata
	}
	if result.Thumbnail != nil {
		if err := m.validateCommittedThumbnail(ctx, result.Thumbnail); err != nil {
			result.Warnings = append(result.Warnings, domain.Warning{Code: "thumbnail_recovery_failed", Message: "video artifact recovered but thumbnail was not available"})
			result.Thumbnail = nil
		}
	}
	if result.Operation == "" {
		if progress.Kind == "image" {
			result.Operation = domain.OperationNormalized
		} else {
			result.Operation = domain.OperationTranscoded
		}
	}
	return result, nil
}

func extensionForKind(kind string) string {
	if kind == "image" {
		return ".jpg"
	}
	if kind == "audio" {
		return ".ogg"
	}
	return ".mp4"
}

func mimeForKind(kind string) string {
	if kind == "image" {
		return "image/jpeg"
	}
	if kind == "audio" {
		return "audio/ogg"
	}
	return "video/mp4"
}

func (m *Manager) validateCommittedOutput(ctx context.Context, output *domain.OutputMetadata) error {
	started := time.Now()
	defer m.recordPhase(observability.PhaseValidation, started)
	artifact, err := m.store.OpenCommitted(ctx, output.ArtifactID)
	if err != nil {
		return err
	}
	if artifact.File != nil {
		defer artifact.File.Close()
	}
	hash, size, err := processor.HashReader(artifact.File)
	if err != nil || size != output.Size || !strings.EqualFold(hash, output.SHA256) {
		return fmt.Errorf("artifact hash or size mismatch")
	}
	return nil
}

func (m *Manager) validateCommittedThumbnail(ctx context.Context, thumbnail *domain.ThumbnailMetadata) error {
	started := time.Now()
	defer m.recordPhase(observability.PhaseValidation, started)
	artifact, err := m.store.OpenCommitted(ctx, thumbnail.ArtifactID)
	if err != nil {
		return err
	}
	if artifact.File != nil {
		defer artifact.File.Close()
	}
	hash, size, err := processor.HashReader(artifact.File)
	if err != nil || size != thumbnail.Size || !strings.EqualFold(hash, thumbnail.SHA256) {
		return fmt.Errorf("thumbnail hash or size mismatch")
	}
	return nil
}

func (m *Manager) validateImageOutput(ctx context.Context, path string) (domain.OutputMetadata, error) {
	started := time.Now()
	defer m.recordPhase(observability.PhaseValidation, started)
	return m.images.Validate(ctx, path)
}

func (m *Manager) validateVideoOutput(ctx context.Context, path string) (domain.OutputMetadata, error) {
	started := time.Now()
	defer m.recordPhase(observability.PhaseValidation, started)
	return m.videos.Validate(ctx, path)
}

func (m *Manager) validateAudioOutput(ctx context.Context, path, policy string) (domain.OutputMetadata, error) {
	started := time.Now()
	defer m.recordPhase(observability.PhaseValidation, started)
	return m.audio.Validate(ctx, path, policy)
}

func (m *Manager) recordPhase(phase string, started time.Time) {
	duration := time.Since(started)
	m.metrics.RecordPhase(phase, duration)
	m.logger.Debug("phase_duration", "phase", phase, "duration_ms", duration.Milliseconds())
}

func (m *Manager) SetLogger(logger *slog.Logger) {
	if logger != nil {
		m.logger = logger
	}
}

func (m *Manager) Metrics() *observability.Metrics { return m.metrics }

func (m *Manager) Submit(ctx context.Context, request domain.JobRequest) (domain.JobRecord, bool, error) {
	request = request.Normalized()
	m.Start()
	if err := validateRequest(request, m.cfg); err != nil {
		return domain.JobRecord{}, false, err
	}
	hash := state.RequestHash(request)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, err := m.states.Get(ctx, request.JobID); err == nil {
		if existing.RequestHash != hash {
			return domain.JobRecord{}, false, domain.NewError(domain.CodeConflict, "job_id already exists with a different request", "request", false, errors.New("conflict"))
		}
		m.metrics.IncIdempotent()
		return existing, true, nil
	} else if !errors.Is(err, state.ErrNotFound) {
		return domain.JobRecord{}, false, err
	}
	record := domain.JobRecord{JobID: request.JobID, Request: request, RequestHash: hash, State: domain.JobQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Processor: domain.ProcessorInfo{Version: m.cfg.ProcessorVersion, Policy: m.cfg.PolicyVersion, ToolVersions: m.cfg.ToolVersions}}
	if err := m.states.Put(ctx, record); err != nil {
		m.metrics.RecordError("storage_write_failed")
		return domain.JobRecord{}, false, err
	}
	m.queueAt.Store(request.JobID, time.Now())
	select {
	case m.queue <- request.JobID:
		m.metrics.IncJobSubmitted()
		m.metrics.SetQueueDepth(len(m.queue))
		m.logger.Info("job_queued", "job_id", record.JobID, "item_count", len(record.Request.Items))
		return record, false, nil
	default:
		m.queueAt.Delete(request.JobID)
		m.metrics.IncQueueRejected()
		m.logger.Warn("job_rejected", "job_id", record.JobID, "reason", "queue_full")
		if err := m.states.Delete(ctx, request.JobID); err != nil {
			m.metrics.RecordError("storage_write_failed")
			return domain.JobRecord{}, false, domain.NewError("storage_write_failed", "could not remove rejected queued job", "queue", true, err)
		}
		return domain.JobRecord{}, false, domain.NewError("queue_full", "job queue is full", "queue", true, nil)
	}
}

func (m *Manager) Get(ctx context.Context, jobID string) (domain.JobRecord, error) {
	return m.states.Get(ctx, jobID)
}
func (m *Manager) QueueDepth() int { return len(m.queue) }
func (m *Manager) Stop()           { m.StopWithContext(context.Background()) }
func (m *Manager) StopWithContext(ctx context.Context) {
	m.cancel()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		m.logger.Warn("job_workers_shutdown_timeout", "error", ctx.Err())
	}
}
func (m *Manager) WorkerCount() int { return m.cfg.JobWorkers }

func (m *Manager) Cleanup(ctx context.Context) (err error) {
	defer func() {
		m.metrics.RecordCleanup(err)
		if err != nil {
			m.logger.Warn("cleanup_failed", "error", err)
		}
	}()
	keep := make(map[string]struct{})
	inputKeep := make(map[string]struct{})
	records, err := m.states.List(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.State != domain.JobCompleted {
			for _, item := range record.Request.Items {
				if item.ArtifactID != "" {
					inputKeep[item.ArtifactID] = struct{}{}
				}
			}
		}
		for _, progress := range record.Progress {
			if progress.PlannedOutputID != "" {
				keep[progress.PlannedOutputID] = struct{}{}
			}
			if progress.PlannedThumbnailID != "" {
				keep[progress.PlannedThumbnailID] = struct{}{}
			}
			if progress.OutputArtifactID != "" {
				keep[progress.OutputArtifactID] = struct{}{}
			}
			if progress.ThumbnailArtifactID != "" {
				keep[progress.ThumbnailArtifactID] = struct{}{}
			}
		}
		if record.Result == nil {
			continue
		}
		for _, item := range record.Result.Items {
			if item.Output != nil {
				keep[item.Output.ArtifactID] = struct{}{}
			}
			if item.Thumbnail != nil {
				keep[item.Thumbnail.ArtifactID] = struct{}{}
			}
		}
	}
	if m.cache != nil {
		refs, err := m.cache.ReferencedArtifacts(ctx)
		if err != nil {
			return err
		}
		for id := range refs {
			keep[id] = struct{}{}
		}
	}
	if err := m.store.CleanupOldExcept(ctx, m.cfg.ArtifactRetention, keep); err != nil {
		return err
	}
	stagingRetention := m.cfg.StagingRetention
	if stagingRetention <= 0 {
		stagingRetention = 7 * 24 * time.Hour
	}
	if err := m.store.CleanupStagedOldExcept(ctx, stagingRetention, inputKeep); err != nil {
		return err
	}
	if err := m.states.CleanupOld(ctx, m.cfg.StateRetention); err != nil {
		return err
	}
	if m.cache != nil {
		if err := m.cache.CleanupOld(ctx, m.cfg.CacheRetention); err != nil {
			return err
		}
	}
	return storage.CleanupOld(m.cfg.WorkRoot, m.cfg.WorkspaceRetention)
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case jobID := <-m.queue:
			queueStarted := time.Now()
			if value, ok := m.queueAt.LoadAndDelete(jobID); ok {
				if started, ok := value.(time.Time); ok {
					queueStarted = started
				}
			}
			m.recordPhase(observability.PhaseQueue, queueStarted)
			m.metrics.SetQueueDepth(len(m.queue))
			m.processJob(jobID)
		}
	}
}

func (m *Manager) processJob(jobID string) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.JobTimeout)
	defer cancel()
	stateStarted := time.Now()
	record, err := m.states.Get(ctx, jobID)
	m.recordPhase(observability.PhaseStateIO, stateStarted)
	if err != nil {
		m.logger.Error("job_load_failed", "job_id", jobID, "error", err)
		m.metrics.RecordError("state_read_failed")
		return
	}
	m.logger.Info("job_processing", "job_id", jobID, "item_count", len(record.Request.Items))
	record.State, record.UpdatedAt = domain.JobProcessing, time.Now().UTC()
	if err := m.persistState(context.Background(), record); err != nil {
		m.logger.Error("job_processing_state_persist_failed", "job_id", jobID, "error", err)
		m.metrics.RecordError("state_write_failed")
		return
	}
	record.Progress = make([]domain.ItemProgress, len(record.Request.Items))
	for index, item := range record.Request.Items {
		record.Progress[index] = domain.ItemProgress{ID: item.ID, State: domain.ProgressPending}
	}
	if err := m.persistState(context.Background(), record); err != nil {
		m.logger.Error("job_progress_init_persist_failed", "job_id", jobID, "error", err)
		m.metrics.RecordError("state_write_failed")
		return
	}
	tracker := newProgressTracker(m, record)
	results := make([]domain.ItemResult, len(record.Request.Items))
	budget := &jobBudget{}
	resultCh := make(chan indexedResult, len(results))
	itemJobs := make(chan indexedResult, len(results))
	var itemWG sync.WaitGroup
	workers := m.cfg.MaxConcurrentItemsPerJob
	if workers > len(record.Request.Items) {
		workers = len(record.Request.Items)
	}
	for i := 0; i < workers; i++ {
		itemWG.Add(1)
		go func() {
			defer itemWG.Done()
			for item := range itemJobs {
				_ = tracker.update(context.Background(), item.index, func(progress *domain.ItemProgress) {
					progress.State = domain.ProgressProcessing
				})
				item.result = m.processItem(ctx, record, record.Request.Items[item.index], item.index, budget, tracker)
				_ = m.persistItemProgress(context.Background(), tracker, item.index, item.result)
				m.metrics.RecordItem(string(item.result.Status), string(item.result.Operation), errorCode(item.result.Error), inputSize(item.result.Input), outputSize(item.result.Output))
				m.logger.Info("job_item_completed", "job_id", record.JobID, "item_id", item.result.ID, "input_sha256", inputHash(item.result.Input), "detected_format", detectedFormat(item.result.Detected), "status", item.result.Status, "operation", item.result.Operation, "input_size", inputSize(item.result.Input), "output_size", outputSize(item.result.Output), "error_code", errorCode(item.result.Error))
				resultCh <- item
			}
		}()
	}
	for index := range record.Request.Items {
		itemJobs <- indexedResult{index: index}
	}
	close(itemJobs)
	for completed := 0; completed < len(results); completed++ {
		result := <-resultCh
		results[result.index] = result.result
	}
	itemWG.Wait()
	record = tracker.snapshot()
	jobResult := domain.JobResult{Items: results}
	jobResult.Outcome = jobResult.Aggregate()
	record.Result, record.State, record.UpdatedAt = &jobResult, domain.JobCompleted, time.Now().UTC()
	if err := m.persistState(context.Background(), record); err != nil {
		m.logger.Error("job_result_persist_failed", "job_id", jobID, "outcome", jobResult.Outcome, "error", err)
		m.metrics.RecordError("state_write_failed")
		return
	}
	m.metrics.RecordJobCompleted(string(jobResult.Outcome), time.Since(started))
	m.logger.Info("job_completed", "job_id", jobID, "outcome", jobResult.Outcome, "duration_ms", time.Since(started).Milliseconds())
}

func (m *Manager) persistState(parent context.Context, record domain.JobRecord) error {
	started := time.Now()
	defer m.recordPhase(observability.PhaseStateIO, started)
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = m.states.Put(ctx, record)
		if err == nil {
			return nil
		}
		if attempt == 2 {
			break
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			if err == nil {
				return ctx.Err()
			}
			return err
		case <-timer.C:
		}
	}
	return err
}

type indexedResult struct {
	index  int
	result domain.ItemResult
}

type jobBudget struct {
	inputBytes atomic.Int64
	pixels     atomic.Int64
	durationMS atomic.Int64
}

func (b *jobBudget) reserveInput(size, limit int64) bool {
	value := b.inputBytes.Add(size)
	if value <= limit {
		return true
	}
	b.inputBytes.Add(-size)
	return false
}

func (b *jobBudget) reserveMedia(pixels int64, duration time.Duration, pixelLimit int64, durationLimit time.Duration) bool {
	newPixels := b.pixels.Add(pixels)
	newDuration := b.durationMS.Add(duration.Milliseconds())
	if newPixels <= pixelLimit && newDuration <= durationLimit.Milliseconds() {
		return true
	}
	b.pixels.Add(-pixels)
	b.durationMS.Add(-duration.Milliseconds())
	return false
}

func (m *Manager) detectMedia(ctx context.Context, path string) (detected domain.MediaDetected, err error) {
	started := time.Now()
	defer m.recordPhase(observability.PhaseDetectProbe, started)

	detected, err = m.detector.Detect(ctx, path, m.cfg.MaxInputBytes)
	probed := false
	if err != nil {
		var detectionErr *domain.Error
		if errors.As(err, &detectionErr) && detectionErr.Code == "unsupported_format" {
			detected, err = m.videos.Probe.Probe(ctx, path)
			probed = true
		}
	}
	if err != nil {
		return domain.MediaDetected{}, err
	}
	if !probed && (detected.Kind == "video" || detected.Kind == "audio") {
		probedMedia, probeErr := m.videos.Probe.Probe(ctx, path)
		if probeErr != nil {
			return domain.MediaDetected{}, probeErr
		}
		probedMedia.Container = detected.Container
		probedMedia.MIME = detected.MIME
		detected = probedMedia
	}
	return detected, nil
}

func (m *Manager) processItem(ctx context.Context, record domain.JobRecord, item domain.Item, index int, budget *jobBudget, tracker *progressTracker) domain.ItemResult {
	result := domain.ItemResult{ID: item.ID, Status: domain.ItemFailed}
	workspace, err := storage.NewWorkspace(m.cfg.WorkRoot, record.JobID, fmt.Sprintf("%03d-%s", index, item.ID))
	if err != nil {
		result.Error = asProcessingError(err)
		return result
	}
	defer workspace.Close()
	input, err := m.store.OpenInput(ctx, item.ArtifactID)
	if err != nil {
		result.Status, result.Error = statusError(err)
		return result
	}
	if input.File != nil {
		defer input.File.Close()
	}
	if !budget.reserveInput(input.Size, m.cfg.MaxAggregateInputBytes) {
		result.Status = domain.ItemRejected
		result.Error = asProcessingError(domain.NewError("input_size_exceeded", "job input size exceeds configured aggregate limit", "limits", false, nil))
		return result
	}
	inputPath := filepath.Join(workspace.Path, "input.bin")
	if err := copyLimitedReader(input.File, inputPath, m.cfg.MaxInputBytes); err != nil {
		result.Status, result.Error = statusError(err)
		return result
	}
	hash, size, err := processor.HashFile(inputPath)
	if err != nil {
		result.Error = asProcessingError(domain.NewError("storage_read_failed", "could not hash input", "hash", true, err))
		return result
	}
	result.Input = &domain.InputMetadata{SHA256: hash, Size: size}
	if item.ExpectedSHA256 != "" && !strings.EqualFold(item.ExpectedSHA256, hash) {
		result.Status = domain.ItemRejected
		result.Error = asProcessingError(domain.NewError("input_hash_mismatch", "input SHA-256 does not match expected hash", "hash", false, nil))
		return result
	}
	detected, err := m.detectMedia(ctx, inputPath)
	if err != nil {
		result.Status, result.Error = statusError(err)
		return result
	}
	result.Detected = &detected
	if detected.Kind == "audio" && !supportedAudioInput(detected) {
		result.Status = domain.ItemRejected
		result.Error = asProcessingError(domain.NewError("unsupported_format", "audio input container is not supported", "detect", false, nil))
		return result
	}
	if !budget.reserveMedia(int64(detected.Width)*int64(detected.Height), time.Duration(detected.DurationMS)*time.Millisecond, m.cfg.MaxAggregatePixels, m.cfg.MaxAggregateDuration) {
		result.Status = domain.ItemRejected
		result.Error = asProcessingError(domain.NewError("output_size_exceeded", "job media budget exceeds configured aggregate limit", "limits", false, nil))
		return result
	}
	if item.DeclaredKind != "" && !strings.EqualFold(item.DeclaredKind, detected.Kind) {
		if record.Request.Policy.StrictFormatMatch {
			result.Status = domain.ItemRejected
			result.Error = asProcessingError(domain.NewError("format_mismatch", "declared kind differs from detected content", "detect", false, nil))
			return result
		}
		result.Warnings = append(result.Warnings, domain.Warning{Code: "format_mismatch", Message: "declared kind differs from detected content; detected content was used"})
	}
	if extensionMismatch(item.ArtifactID, detected) {
		if record.Request.Policy.StrictFormatMatch {
			result.Status = domain.ItemRejected
			result.Error = asProcessingError(domain.NewError("format_mismatch", "artifact extension differs from detected content", "detect", false, nil))
			return result
		}
		result.Warnings = append(result.Warnings, domain.Warning{Code: "format_mismatch", Message: "artifact extension differs from detected content; detected content was used"})
	}
	key := m.cacheKey(hash, detected, record.Request.Policy)
	result.CacheKey = key
	if cached := m.readCachedResult(ctx, key, item.ID, result.Input, &detected); cached != nil {
		cached.Warnings = append(cached.Warnings, result.Warnings...)
		return *cached
	}
	if detected.Kind == "image" {
		return m.processImage(ctx, record, result, inputPath, detected, workspace.Path, tracker, index)
	}
	if detected.Kind == "audio" {
		return m.processAudio(ctx, record, result, inputPath, detected, workspace.Path, tracker, index)
	}
	return m.processVideo(ctx, record, result, inputPath, detected, workspace.Path, tracker, index)
}

func (m *Manager) persistItemProgress(ctx context.Context, tracker *progressTracker, index int, result domain.ItemResult) error {
	if result.Status == domain.ItemSuccess && tracker.hasCommittedResult(index, result) {
		return nil
	}
	return tracker.update(ctx, index, func(progress *domain.ItemProgress) {
		progress.Result = &result
		progress.Input = result.Input
		progress.Detected = result.Detected
		progress.Operation = result.Operation
		if result.Output != nil {
			progress.OutputArtifactID = result.Output.ArtifactID
		}
		if result.Thumbnail != nil {
			progress.ThumbnailArtifactID = result.Thumbnail.ArtifactID
		}
		switch result.Status {
		case domain.ItemSuccess:
			progress.State = domain.ProgressCommitted
		case domain.ItemRejected:
			progress.State = domain.ProgressRejected
		default:
			progress.State = domain.ProgressFailed
		}
	})
}

func (m *Manager) processImage(ctx context.Context, record domain.JobRecord, result domain.ItemResult, input string, detected domain.MediaDetected, workspace string, tracker *progressTracker, index int) domain.ItemResult {
	select {
	case m.imageSem <- struct{}{}:
		defer func() { <-m.imageSem }()
	case <-ctx.Done():
		result.Error = asProcessingError(ctx.Err())
		return result
	}
	out := filepath.Join(workspace, "output.jpg")
	processingStarted := time.Now()
	operation, _, err := m.images.Process(ctx, input, out, record.Request.Policy)
	m.recordPhase(observability.PhaseProcessing, processingStarted)
	if err != nil {
		result.Status, result.Error = statusError(err)
		return result
	}
	metadata, err := m.validateImageOutput(ctx, out)
	if err != nil {
		result.Status, result.Error = statusError(err)
		return result
	}
	result.Transform = detected.Format + "_to_jpeg"
	if detected.Format == "jpeg" {
		result.Transform = "jpeg_normalize"
	}
	result = m.commitResult(ctx, result, operation, out, metadata, "image", tracker, index)
	m.writeCachedResult(ctx, result)
	return result
}

func (m *Manager) processVideo(ctx context.Context, record domain.JobRecord, result domain.ItemResult, input string, detected domain.MediaDetected, workspace string, tracker *progressTracker, index int) domain.ItemResult {
	select {
	case m.videoSem <- struct{}{}:
		defer func() { <-m.videoSem }()
	case <-ctx.Done():
		result.Error = asProcessingError(ctx.Err())
		return result
	}
	out := filepath.Join(workspace, "output.mp4")
	processingStarted := time.Now()
	operation, _, err := m.videos.Process(ctx, input, out, detected)
	m.recordPhase(observability.PhaseProcessing, processingStarted)
	if err != nil {
		result.Status, result.Error = statusError(err)
		return result
	}
	metadata, err := m.validateVideoOutput(ctx, out)
	if err != nil {
		result.Status, result.Error = statusError(err)
		return result
	}
	result = m.commitResult(ctx, result, operation, out, metadata, "video", tracker, index)
	if result.Status != domain.ItemSuccess || !record.Request.Policy.GenerateThumbnail {
		m.writeCachedResult(ctx, result)
		return result
	}
	thumbPath := filepath.Join(workspace, "thumbnail.jpg")
	thumb, thumbErr := m.videos.Thumbnail(ctx, out, thumbPath)
	if thumbErr != nil {
		result.Warnings = append(result.Warnings, domain.Warning{Code: "thumbnail_generation_failed", Message: "video succeeded but thumbnail generation failed"})
		m.writeCachedResult(ctx, result)
		return result
	}
	thumbID := storage.NewArtifactID("thumb")
	_ = tracker.update(context.Background(), index, func(progress *domain.ItemProgress) {
		progress.PlannedThumbnailID = thumbID
	})
	final, tmp, err := m.store.Begin(ctx, thumbID, ".jpg")
	if err != nil {
		result.Warnings = append(result.Warnings, domain.Warning{Code: "thumbnail_generation_failed", Message: "video succeeded but thumbnail storage failed"})
		m.writeCachedResult(ctx, result)
		return result
	}
	if err := m.store.CopyOutput(ctx, thumbPath, tmp, m.cfg.MaxOutputBytes); err != nil {
		_ = os.Remove(tmp)
		result.Warnings = append(result.Warnings, domain.Warning{Code: "thumbnail_generation_failed", Message: "video succeeded but thumbnail storage failed"})
		m.writeCachedResult(ctx, result)
		return result
	}
	committed, err := m.store.Commit(ctx, tmp, final)
	if err != nil {
		_ = os.Remove(tmp)
		result.Warnings = append(result.Warnings, domain.Warning{Code: "thumbnail_generation_failed", Message: "video succeeded but thumbnail commit failed"})
		m.writeCachedResult(ctx, result)
		return result
	}
	thash, _, hashErr := processor.HashFile(committed.Path)
	if hashErr != nil {
		_ = m.store.Remove(ctx, committed.ID)
		result.Warnings = append(result.Warnings, domain.Warning{Code: "thumbnail_generation_failed", Message: "video succeeded but thumbnail integrity verification failed"})
		m.writeCachedResult(ctx, result)
		return result
	}
	thumb.SHA256, thumb.ArtifactID, thumb.Filename = thash, committed.ID, filepath.Base(committed.Path)
	result.Thumbnail = &domain.ThumbnailMetadata{ArtifactID: committed.ID, Filename: filepath.Base(committed.Path), Extension: ".jpg", MIME: "image/jpeg", Size: committed.Size, Width: thumb.Width, Height: thumb.Height, SHA256: thash}
	_ = tracker.update(context.Background(), index, func(progress *domain.ItemProgress) {
		progress.ThumbnailArtifactID = committed.ID
		progress.Result = &result
	})
	m.writeCachedResult(ctx, result)
	return result
}

func (m *Manager) processAudio(ctx context.Context, record domain.JobRecord, result domain.ItemResult, input string, detected domain.MediaDetected, workspace string, tracker *progressTracker, index int) domain.ItemResult {
	select {
	case m.videoSem <- struct{}{}:
		defer func() { <-m.videoSem }()
	case <-ctx.Done():
		result.Error = asProcessingError(ctx.Err())
		return result
	}
	out := filepath.Join(workspace, "output"+audioExtension(record.Request.Policy.TargetAudio))
	processingStarted := time.Now()
	operation, _, err := m.audio.Process(ctx, input, out, record.Request.Policy, detected)
	m.recordPhase(observability.PhaseProcessing, processingStarted)
	if err != nil {
		result.Status, result.Error = statusError(err)
		return result
	}
	metadata, err := m.validateAudioOutput(ctx, out, record.Request.Policy.TargetAudio)
	if err != nil {
		result.Status, result.Error = statusError(err)
		return result
	}
	result.Transform = "audio_normalize"
	result = m.commitResult(ctx, result, operation, out, metadata, "audio", tracker, index)
	m.writeCachedResult(ctx, result)
	return result
}

func audioExtension(policy string) string {
	switch policy {
	case "wav_pcm_s16le":
		return ".wav"
	case "m4a_aac_lc":
		return ".m4a"
	default:
		return ".ogg"
	}
}

func supportedAudioInput(detected domain.MediaDetected) bool {
	switch strings.ToLower(detected.Container) {
	case "ogg", "wav", "m4a":
		return true
	default:
		return false
	}
}

func (m *Manager) cacheKey(inputSHA string, detected domain.MediaDetected, policy domain.Policy) string {
	value := strings.Join([]string{inputSHA, m.cfg.ProcessorVersion, m.cfg.PolicyVersion, m.cfg.ToolFingerprint, policy.TargetImage, policy.TargetVideo, policy.TargetAudio, detected.Kind, detected.Format, detected.Container, detected.VideoCodec, detected.AudioCodec, detected.AudioProfile, detected.PixelFormat, fmt.Sprintf("sample_rate=%d", detected.SampleRate), fmt.Sprintf("channels=%d", detected.Channels), boolString(detected.HasAudio), boolString(policy.StripMetadata), boolString(policy.GenerateThumbnail), policy.AlphaBackground, boolString(policy.StrictFormatMatch), fmt.Sprintf("ffmpeg_threads=%d", m.cfg.FFmpegThreads), "jpeg_quality=90", "h264_crf=23", "h264_preset=medium", "audio_opus_32k", "audio_aac_96k", "thumb=640x360:q3:t1"}, "|")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) readCachedResult(ctx context.Context, key, itemID string, input *domain.InputMetadata, detected *domain.MediaDetected) *domain.ItemResult {
	if m.cache == nil {
		return nil
	}
	data, err := m.cache.Get(ctx, key)
	if err != nil {
		return nil
	}
	var result domain.ItemResult
	encoded, ok := data["result"]
	if !ok || json.Unmarshal(encoded, &result) != nil || result.Status != domain.ItemSuccess || result.Output == nil {
		return nil
	}
	output, err := m.store.OpenCommitted(ctx, result.Output.ArtifactID)
	if err != nil {
		return nil
	}
	if output.File != nil {
		defer output.File.Close()
	}
	if hash, _, err := processor.HashReader(output.File); err != nil || !strings.EqualFold(hash, result.Output.SHA256) || output.Size != result.Output.Size {
		return nil
	}
	if result.Thumbnail != nil {
		thumbnail, err := m.store.OpenCommitted(ctx, result.Thumbnail.ArtifactID)
		if err != nil {
			result.Thumbnail = nil
		} else {
			if hash, _, hashErr := processor.HashReader(thumbnail.File); hashErr != nil || !strings.EqualFold(hash, result.Thumbnail.SHA256) {
				result.Thumbnail = nil
			}
			_ = thumbnail.File.Close()
		}
	}
	result.ID, result.Input, result.Detected, result.CacheKey = itemID, input, detected, key
	return &result
}

func (m *Manager) writeCachedResult(ctx context.Context, result domain.ItemResult) {
	if m.cache == nil || result.Status != domain.ItemSuccess || result.CacheKey == "" {
		return
	}
	_ = m.cache.Put(ctx, result.CacheKey, map[string]any{"result": result})
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func extensionMismatch(artifactID string, detected domain.MediaDetected) bool {
	extension := strings.ToLower(filepath.Ext(artifactID))
	if extension == "" {
		return false
	}
	if detected.Kind == "image" {
		switch detected.Format {
		case "jpeg":
			return extension != ".jpg" && extension != ".jpeg"
		case "png":
			return extension != ".png"
		case "webp":
			return extension != ".webp"
		case "heic", "heif":
			return extension != ".heic" && extension != ".heif"
		case "avif":
			return extension != ".avif"
		}
	}
	if detected.Kind == "video" {
		return extension != ".mov" && extension != ".qt" && extension != ".mp4" && extension != ".m4v"
	}
	return false
}

func (m *Manager) commitResult(ctx context.Context, result domain.ItemResult, operation domain.Operation, output string, metadata domain.OutputMetadata, kind string, tracker *progressTracker, index int) domain.ItemResult {
	started := time.Now()
	defer m.recordPhase(observability.PhaseCommit, started)
	id := storage.NewArtifactID("norm")
	_ = tracker.update(context.Background(), index, func(progress *domain.ItemProgress) {
		progress.Kind = kind
		progress.Operation = operation
		progress.PlannedOutputID = id
	})
	ext := metadata.Extension
	if info, statErr := os.Stat(output); statErr != nil {
		result.Error = asProcessingError(domain.NewError("output_validation_failed", "output artifact is missing", "commit", false, statErr))
		return result
	} else if info.Size() > m.cfg.MaxOutputBytes {
		result.Error = asProcessingError(domain.NewError("output_size_exceeded", "output exceeds configured size limit", "commit", false, nil))
		return result
	}
	final, tmp, err := m.store.Begin(ctx, id, ext)
	if err != nil {
		result.Error = asProcessingError(err)
		return result
	}
	if err := m.store.CopyOutput(ctx, output, tmp, m.cfg.MaxOutputBytes); err != nil {
		_ = os.Remove(tmp)
		result.Error = asProcessingError(domain.NewError("storage_write_failed", "could not stage output artifact", "commit", true, err))
		return result
	}
	hash, _, err := processor.HashFile(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		result.Error = asProcessingError(domain.NewError("storage_read_failed", "could not hash staged output", "commit", true, err))
		return result
	}
	committed, err := m.store.Commit(ctx, tmp, final)
	if err != nil {
		_ = os.Remove(tmp)
		result.Error = asProcessingError(err)
		return result
	}
	metadata.ArtifactID, metadata.Filename, metadata.SHA256 = committed.ID, filepath.Base(committed.Path), hash
	if kind == "image" {
		metadata.Extension, metadata.MIME = ".jpg", "image/jpeg"
	} else if kind == "video" {
		metadata.Extension, metadata.MIME = ".mp4", "video/mp4"
	}
	result.Status, result.Operation, result.Output = domain.ItemSuccess, operation, &metadata
	if err := tracker.update(context.Background(), index, func(progress *domain.ItemProgress) {
		progress.Kind = kind
		progress.Operation = operation
		progress.Input = result.Input
		progress.Detected = result.Detected
		progress.OutputArtifactID = metadata.ArtifactID
		progress.State = domain.ProgressCommitted
		progress.Result = &result
	}); err != nil {
		result.Status = domain.ItemFailed
		result.Error = asProcessingError(domain.NewError("storage_write_failed", "could not persist committed artifact progress", "state", true, err))
	}
	return result
}

func validateRequest(request domain.JobRequest, cfg config.Config) error {
	if request.JobID == "" || len(request.JobID) > 128 || filepath.Base(request.JobID) != request.JobID || strings.ContainsAny(request.JobID, "/\\\x00") {
		return domain.NewError(domain.CodeRequestInvalid, "invalid job_id", "request", false, nil)
	}
	if len(request.Items) == 0 || len(request.Items) > cfg.MaxItemsPerJob {
		return domain.NewError(domain.CodeRequestInvalid, "items count is outside configured limits", "request", false, nil)
	}
	if !request.ValidatePolicy() {
		return domain.NewError(domain.CodeRequestInvalid, "unsupported or invalid policy", "request", false, nil)
	}
	seen := make(map[string]struct{}, len(request.Items))
	for _, item := range request.Items {
		if item.ID == "" || len(item.ID) > 128 || filepath.Base(item.ID) != item.ID || strings.ContainsAny(item.ID, "/\\\x00") || item.ArtifactID == "" || len(item.ArtifactID) > 256 || filepath.Base(item.ArtifactID) != item.ArtifactID || strings.ContainsAny(item.ArtifactID, "/\\\x00") {
			return domain.NewError(domain.CodeRequestInvalid, "invalid item identity", "request", false, nil)
		}
		if item.ExpectedSHA256 != "" && !domain.IsSHA256(item.ExpectedSHA256) {
			return domain.NewError(domain.CodeRequestInvalid, "expected_sha256 must be a SHA-256 hex digest", "request", false, nil)
		}
		if _, ok := seen[item.ID]; ok {
			return domain.NewError(domain.CodeRequestInvalid, "item IDs must be unique", "request", false, nil)
		}
		seen[item.ID] = struct{}{}
	}
	return nil
}

func copyLimitedReader(in io.Reader, dst string, limit int64) error {
	if in == nil {
		return domain.NewError("storage_read_failed", "input handle is unavailable", "read", true, nil)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return domain.NewError("input_size_exceeded", "input exceeds configured size limit", "read", false, nil)
	}
	return nil
}
func statusError(err error) (domain.ItemStatus, *domain.ProcessingError) {
	pe := asProcessingError(err)
	if pe.Code == "input_missing" || pe.Code == "artifact_not_found" || pe.Code == "invalid_artifact_id" || pe.Code == "input_not_file" || pe.Code == "unsupported_format" || pe.Code == "unsupported_animation" || pe.Code == "format_mismatch" || pe.Code == "input_hash_mismatch" || pe.Code == "corrupt_media" || pe.Code == "input_size_exceeded" || pe.Code == "policy_input_kind_mismatch" || pe.Code == "audio_probe_failed" || strings.HasPrefix(pe.Code, "image_") || pe.Code == "unsupported_video_codec" {
		return domain.ItemRejected, pe
	}
	return domain.ItemFailed, pe
}
func asProcessingError(err error) *domain.ProcessingError {
	var de *domain.Error
	if errors.As(err, &de) {
		return &domain.ProcessingError{Code: de.Code, Message: de.Message, Retryable: de.Retryable, Stage: de.Stage}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &domain.ProcessingError{Code: "timeout", Message: "processing timed out", Retryable: false, Stage: "process"}
	}
	if errors.Is(err, context.Canceled) {
		return &domain.ProcessingError{Code: "timeout", Message: "processing was canceled", Retryable: true, Stage: "process"}
	}
	return &domain.ProcessingError{Code: "internal_error", Message: "internal processing error", Retryable: false, Stage: "process"}
}
func failedItems(items []domain.Item, err *domain.Error) []domain.ItemResult {
	results := make([]domain.ItemResult, len(items))
	for i, item := range items {
		results[i] = domain.ItemResult{ID: item.ID, Status: domain.ItemFailed, Error: asProcessingError(err)}
	}
	return results
}

func errorCode(err *domain.ProcessingError) string {
	if err == nil {
		return ""
	}
	return err.Code
}

func inputHash(input *domain.InputMetadata) string {
	if input == nil {
		return ""
	}
	return input.SHA256
}

func detectedFormat(detected *domain.MediaDetected) string {
	if detected == nil {
		return ""
	}
	if detected.Format != "" {
		return detected.Format
	}
	return detected.Container
}

func inputSize(input *domain.InputMetadata) int64 {
	if input == nil {
		return 0
	}
	return input.Size
}

func outputSize(output *domain.OutputMetadata) int64 {
	if output == nil {
		return 0
	}
	return output.Size
}
