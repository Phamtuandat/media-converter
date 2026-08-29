package job

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"media-converter-v2/internal/config"
	"media-converter-v2/internal/domain"
	"media-converter-v2/internal/processor"
	"media-converter-v2/internal/state"
	"media-converter-v2/internal/storage"
)

type failingStateStore struct {
	base     *state.Store
	failFrom int
	puts     int
}

func (s *failingStateStore) Get(ctx context.Context, jobID string) (domain.JobRecord, error) {
	return s.base.Get(ctx, jobID)
}

func (s *failingStateStore) Put(ctx context.Context, record domain.JobRecord) error {
	s.puts++
	if s.failFrom > 0 && s.puts >= s.failFrom {
		return errors.New("injected state write failure")
	}
	return s.base.Put(ctx, record)
}

func (s *failingStateStore) Delete(ctx context.Context, jobID string) error {
	return s.base.Delete(ctx, jobID)
}

func (s *failingStateStore) List(ctx context.Context) ([]domain.JobRecord, error) {
	return s.base.List(ctx)
}

func (s *failingStateStore) CleanupOld(ctx context.Context, age time.Duration) error {
	return s.base.CleanupOld(ctx, age)
}

func TestSubmitIdempotencyAndConflict(t *testing.T) {
	input, output, stateRoot := t.TempDir(), t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{QueueSize: 2, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir(), MaxInputBytes: 10, MaxOutputBytes: 10}
	manager := NewManager(cfg, store, states)
	defer manager.Stop()
	request := domain.JobRequest{JobID: "job-idempotent", Items: []domain.Item{{ID: "item-1", ArtifactID: "input-1"}}, Policy: domain.DefaultPolicy()}
	first, existing, err := manager.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if existing {
		t.Fatal("first submit was existing")
	}
	second, existing, err := manager.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !existing || second.JobID != first.JobID {
		t.Fatalf("unexpected idempotent response: %+v, %v", second, existing)
	}
	request.Policy.StrictFormatMatch = true
	if _, _, err := manager.Submit(context.Background(), request); err == nil {
		t.Fatal("expected conflicting request to fail")
	}
}

func TestCleanupKeepsDurablyReferencedStagingArtifact(t *testing.T) {
	input, output, stateRoot := t.TempDir(), t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.Stage(context.Background(), bytes.NewReader([]byte("staged")), 1024)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(staged.Path, old, old); err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	record := domain.JobRecord{JobID: "queued", State: domain.JobQueued, Request: domain.JobRequest{JobID: "queued", Items: []domain.Item{{ID: "item", ArtifactID: staged.ID}}}}
	if err := states.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir(), StagingRetention: time.Nanosecond, StateRetention: 24 * time.Hour, ArtifactRetention: 24 * time.Hour, CacheRetention: 24 * time.Hour, WorkspaceRetention: 24 * time.Hour, JanitorInterval: time.Hour}
	manager := NewManager(cfg, store, states)
	defer manager.Stop()
	if err := manager.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenInput(context.Background(), staged.ID); err != nil {
		t.Fatalf("durably referenced staging artifact was removed: %v", err)
	}
	record.State = domain.JobCompleted
	if err := states.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := manager.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenInput(context.Background(), staged.ID); err == nil {
		t.Fatal("unreferenced completed staging artifact was not removed")
	}
}

func TestSubmitRejectsDuplicateItemIDs(t *testing.T) {
	store, err := storage.NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}
	manager := NewManager(cfg, store, states)
	defer manager.Stop()
	_, _, err = manager.Submit(context.Background(), domain.JobRequest{
		JobID: "duplicate-items",
		Items: []domain.Item{{ID: "same", ArtifactID: "one"}, {ID: "same", ArtifactID: "two"}},
	})
	if err == nil {
		t.Fatal("expected duplicate item IDs to be rejected")
	}
	if got := asProcessingError(err).Code; got != domain.CodeRequestInvalid {
		t.Fatalf("got error code %q", got)
	}
}

func TestSubmitRejectsInvalidExpectedHash(t *testing.T) {
	store, err := storage.NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}, store, states)
	defer manager.Stop()
	_, _, err = manager.Submit(context.Background(), domain.JobRequest{JobID: "bad-hash", Items: []domain.Item{{ID: "item", ArtifactID: "input", ExpectedSHA256: "bad"}}})
	if err == nil || asProcessingError(err).Code != domain.CodeRequestInvalid {
		t.Fatalf("expected request_invalid, got %v", err)
	}
}

func TestRecoverStartsWorkersBeforeFillingQueue(t *testing.T) {
	store, err := storage.NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		jobID := "recovery-job-" + string(rune('a'+i))
		request := domain.JobRequest{JobID: jobID, Items: []domain.Item{{ID: "item", ArtifactID: "missing-input"}}, Policy: domain.DefaultPolicy()}
		record := domain.JobRecord{JobID: jobID, Request: request, RequestHash: state.RequestHash(request), State: domain.JobQueued, CreatedAt: now, UpdatedAt: now}
		if err := states.Put(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir(), JobTimeout: time.Second}
	manager := NewManager(cfg, store, states)
	defer manager.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < 5; i++ {
		jobID := "recovery-job-" + string(rune('a'+i))
		for {
			record, err := manager.Get(context.Background(), jobID)
			if err != nil {
				t.Fatal(err)
			}
			if record.State == domain.JobCompleted && record.Result != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("job %q was not completed after recovery: %+v", jobID, record)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestProcessJobLogsAndLeavesProcessingStateWhenResultPersistFails(t *testing.T) {
	store, err := storage.NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baseStates, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := domain.JobRequest{JobID: "persist-failure", Items: []domain.Item{{ID: "item", ArtifactID: "missing-input"}}, Policy: domain.DefaultPolicy()}
	record := domain.JobRecord{JobID: request.JobID, Request: request, RequestHash: state.RequestHash(request), State: domain.JobQueued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := baseStates.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	states := &failingStateStore{base: baseStates, failFrom: 2}
	manager := NewManager(config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir(), JobTimeout: time.Second}, store, states)
	manager.Start()
	manager.queue <- request.JobID
	defer manager.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := baseStates.Get(context.Background(), request.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == domain.JobProcessing {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := baseStates.Get(context.Background(), request.JobID)
	t.Fatalf("expected processing state to remain recoverable, got %+v", got)
}

func TestRecoverReconcilesCommittedArtifactWithoutReprocessing(t *testing.T) {
	input, output, stateRoot := t.TempDir(), t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("committed-artifact")
	final, tmp, err := store.Begin(context.Background(), "norm-reconcile", ".jpg")
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
	hash, size, err := processor.HashFile(committed.Path)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.JobRequest{JobID: "reconcile-success", Items: []domain.Item{{ID: "item", ArtifactID: "input.jpg"}}, Policy: domain.DefaultPolicy()}
	record := domain.JobRecord{JobID: request.JobID, Request: request, RequestHash: state.RequestHash(request), State: domain.JobProcessing, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Progress: []domain.ItemProgress{{ID: "item", State: domain.ProgressCommitted, Kind: "image", Operation: domain.OperationNormalized, OutputArtifactID: committed.ID, Result: &domain.ItemResult{ID: "item", Status: domain.ItemSuccess, Operation: domain.OperationNormalized, Output: &domain.OutputMetadata{ArtifactID: committed.ID, Extension: ".jpg", MIME: "image/jpeg", Size: size, SHA256: hash}}}}}
	if err := states.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}, store, states)
	defer manager.Stop()
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := manager.Get(context.Background(), request.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobCompleted || got.Result == nil || got.Result.Items[0].Output == nil || got.Result.Items[0].Output.ArtifactID != committed.ID {
		t.Fatalf("unexpected reconciled record: %+v", got)
	}
}

func TestRecoverMarksCorruptCommittedArtifactFailed(t *testing.T) {
	input, output, stateRoot := t.TempDir(), t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	final, tmp, err := store.Begin(context.Background(), "norm-corrupt", ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	committed, err := store.Commit(context.Background(), tmp, final)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.JobRequest{JobID: "reconcile-fail", Items: []domain.Item{{ID: "item", ArtifactID: "input.jpg"}}, Policy: domain.DefaultPolicy()}
	record := domain.JobRecord{JobID: request.JobID, Request: request, RequestHash: state.RequestHash(request), State: domain.JobProcessing, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Progress: []domain.ItemProgress{{ID: "item", State: domain.ProgressCommitted, Kind: "image", OutputArtifactID: committed.ID, Result: &domain.ItemResult{ID: "item", Status: domain.ItemSuccess, Output: &domain.OutputMetadata{ArtifactID: committed.ID, Extension: ".jpg", MIME: "image/jpeg", Size: 7, SHA256: strings.Repeat("0", 64)}}}}}
	if err := states.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}, store, states)
	defer manager.Stop()
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := manager.Get(context.Background(), request.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobCompleted || got.Result == nil || got.Result.Outcome != domain.OutcomeFailed || got.Result.Items[0].Error == nil || got.Result.Items[0].Error.Code != "output_validation_failed" {
		t.Fatalf("unexpected corrupt recovery result: %+v", got)
	}
	if _, err := store.OpenCommitted(context.Background(), committed.ID); err != nil {
		t.Fatal("corrupt artifact should not be replaced or removed")
	}
}

func TestRecoverWithoutCommittedArtifactMarksInterrupted(t *testing.T) {
	store, err := storage.NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := domain.JobRequest{JobID: "reconcile-interrupted", Items: []domain.Item{{ID: "item", ArtifactID: "input.jpg"}}, Policy: domain.DefaultPolicy()}
	record := domain.JobRecord{JobID: request.JobID, Request: request, RequestHash: state.RequestHash(request), State: domain.JobProcessing, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Progress: []domain.ItemProgress{{ID: "item", State: domain.ProgressProcessing, Kind: "image", PlannedOutputID: "norm-missing"}}}
	if err := states.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}, store, states)
	defer manager.Stop()
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := manager.Get(context.Background(), request.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Items[0].Error == nil || got.Result.Items[0].Error.Code != "recovery_interrupted" {
		t.Fatalf("unexpected interrupted recovery result: %+v", got)
	}
}

func TestCleanupPreservesProgressArtifactsBeforeResultPersist(t *testing.T) {
	input, output, stateRoot := t.TempDir(), t.TempDir(), t.TempDir()
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	final, tmp, err := store.Begin(context.Background(), "norm-kept", ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("keep-me"), 0o600); err != nil {
		t.Fatal(err)
	}
	committed, err := store.Commit(context.Background(), tmp, final)
	if err != nil {
		t.Fatal(err)
	}
	request := domain.JobRequest{JobID: "cleanup-progress", Items: []domain.Item{{ID: "item", ArtifactID: "input.jpg"}}, Policy: domain.DefaultPolicy()}
	record := domain.JobRecord{JobID: request.JobID, Request: request, State: domain.JobProcessing, Progress: []domain.ItemProgress{{ID: "item", State: domain.ProgressCommitted, OutputArtifactID: committed.ID}}, UpdatedAt: time.Now().UTC()}
	if err := states.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir(), ArtifactRetention: time.Nanosecond, StateRetention: time.Hour, CacheRetention: time.Hour, WorkspaceRetention: time.Hour}, store, states)
	defer manager.Stop()
	if err := manager.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenCommitted(context.Background(), committed.ID); err != nil {
		t.Fatalf("progress artifact was incorrectly collected: %v", err)
	}
}

func TestRecoverKeepsDurableCompletedJob(t *testing.T) {
	store, err := storage.NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := domain.JobRequest{JobID: "already-completed", Items: []domain.Item{{ID: "item", ArtifactID: "input.jpg"}}, Policy: domain.DefaultPolicy()}
	result := domain.JobResult{Outcome: domain.OutcomeFailed, Items: []domain.ItemResult{{ID: "item", Status: domain.ItemRejected, Error: &domain.ProcessingError{Code: "unsupported_format"}}}}
	record := domain.JobRecord{JobID: request.JobID, Request: request, RequestHash: state.RequestHash(request), State: domain.JobCompleted, Result: &result, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := states.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config.Config{QueueSize: 1, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: t.TempDir()}, store, states)
	defer manager.Stop()
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := manager.Get(context.Background(), request.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.JobCompleted || got.Result == nil || got.Result.Items[0].Error.Code != "unsupported_format" {
		t.Fatalf("durable completed job changed during recovery: %+v", got)
	}
	if manager.QueueDepth() != 0 {
		t.Fatalf("completed job was re-enqueued, queue depth %d", manager.QueueDepth())
	}
}

func TestVideoPipelineWithRealFixture(t *testing.T) {
	if _, err := os.Stat("/opt/homebrew/bin/ffmpeg"); err != nil {
		t.Skip("ffmpeg fixture test requires local ffmpeg")
	}
	input, output, stateRoot, work := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	fixture := filepath.Join(input, "input.mov")
	if err := runFixtureFFmpeg(fixture); err != nil {
		t.Skipf("could not create fixture: %v", err)
	}
	store, err := storage.NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	states, err := state.NewStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{FFmpegPath: "/opt/homebrew/bin/ffmpeg", FFprobePath: "/opt/homebrew/bin/ffprobe", MagickPath: "missing-magick", QueueSize: 2, JobWorkers: 1, ImageWorkers: 1, VideoWorkers: 1, WorkRoot: work, MaxInputBytes: 1 << 20, MaxOutputBytes: 10 << 20, MaxWidth: 1000, MaxHeight: 1000, MaxPixels: 1 << 20, MaxDuration: time.Minute, FFmpegThreads: 1, JobTimeout: time.Minute}
	manager := NewManager(cfg, store, states)
	defer manager.Stop()
	request := domain.JobRequest{JobID: "video-fixture", Items: []domain.Item{{ID: "item-1", ArtifactID: "input.mov"}}, Policy: domain.DefaultPolicy()}
	if _, _, err := manager.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		record, err := manager.Get(context.Background(), request.JobID)
		if err != nil {
			t.Fatal(err)
		}
		if record.State == domain.JobCompleted {
			if record.Result == nil || record.Result.Items[0].Status != domain.ItemSuccess {
				t.Fatalf("unexpected result: %+v", record.Result)
			}
			artifact := record.Result.Items[0].Output
			if artifact == nil || artifact.Extension != ".mp4" {
				t.Fatalf("unexpected output: %+v", artifact)
			}
			if _, err := os.Stat(filepath.Join(output, artifact.ArtifactID+artifact.Extension)); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not complete")
}

func runFixtureFFmpeg(output string) error {
	cmd := execCommand("/opt/homebrew/bin/ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-f", "lavfi", "-i", "sine=frequency=1000", "-t", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", output)
	return cmd.Run()
}

var execCommand = func(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }
