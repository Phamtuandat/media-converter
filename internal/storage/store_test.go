package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocalStoreRejectsUnsafeInputID(t *testing.T) {
	store, err := NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../escape", "/absolute", "nested/file"} {
		if _, err := store.OpenInput(context.Background(), id); err == nil {
			t.Fatalf("expected %q to be rejected", id)
		}
	}
}

func TestLocalStoreCommitsAndOpensArtifact(t *testing.T) {
	store, err := NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	final, tmp, err := store.Begin(context.Background(), "artifact-1", ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	committed, err := store.Commit(context.Background(), tmp, final)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := store.OpenCommitted(context.Background(), committed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Size != 4 || opened.ID != "artifact-1" {
		t.Fatalf("unexpected artifact: %+v", opened)
	}
}

func TestLocalStoreReadsLegacyArtifactWithoutWritingToIt(t *testing.T) {
	input, output, legacy := t.TempDir(), t.TempDir(), t.TempDir()
	legacyPath := filepath.Join(legacy, "legacy-1.jpg")
	if err := os.WriteFile(legacyPath, []byte("old artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewLocalStoreWithModeAndLegacy(input, output, OutputModeLocal, legacy)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.OpenCommitted(context.Background(), "legacy-1")
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.File.Close()
	content, err := io.ReadAll(artifact.File)
	if err != nil || string(content) != "old artifact" {
		t.Fatalf("legacy content = %q, err = %v", content, err)
	}
	entries, err := os.ReadDir(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "legacy-1.jpg" {
		t.Fatalf("legacy root was modified: %v", entries)
	}
}

func TestWebDAVStoreReadsLegacyWhenMountIsUnavailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mount readiness is enforced for the Linux VPS target")
	}
	input, output, legacy := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(legacy, "legacy-2.mp4"), []byte("old video"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewLocalStoreWithModeAndLegacy(input, output, OutputModeWebDAV, legacy)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.OpenCommitted(context.Background(), "legacy-2")
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.File.Close()
	content, err := io.ReadAll(artifact.File)
	if err != nil || string(content) != "old video" {
		t.Fatalf("legacy content = %q, err = %v", content, err)
	}
}

func TestLocalStoreRejectsSymlinkInput(t *testing.T) {
	input, output := t.TempDir(), t.TempDir()
	store, err := NewLocalStore(input, output)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(input, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := store.OpenInput(context.Background(), "linked"); err == nil {
		t.Fatal("expected symlink input to be rejected")
	}
}

func TestLocalStoreStagesArtifactAndHash(t *testing.T) {
	store, err := NewLocalStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("uploaded audio")
	artifact, err := store.Stage(context.Background(), bytes.NewReader(content), 1024)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(content)
	if artifact.ID == "" || artifact.SHA256 != hex.EncodeToString(want[:]) || artifact.Size != int64(len(content)) {
		t.Fatalf("unexpected staged artifact: %+v", artifact)
	}
	opened, err := store.OpenInput(context.Background(), artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.File.Close()
	got, err := io.ReadAll(opened.File)
	if err != nil || string(got) != string(content) {
		t.Fatalf("staged content = %q, err = %v", got, err)
	}
}

func TestWebDAVStoreDoesNotUseUnavailableLocalFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mount readiness is enforced for the Linux VPS target")
	}
	output := t.TempDir()
	store, err := NewLocalStoreWithMode(t.TempDir(), output, OutputModeWebDAV)
	if err != nil {
		t.Fatal(err)
	}
	if store.OutputReady(context.Background()) == nil {
		t.Skip("test directory is unexpectedly a mounted filesystem")
	}
	if _, _, err := store.Begin(context.Background(), "webdav-output", ".jpg"); err == nil {
		t.Fatal("expected output begin to fail without a WebDAV mount")
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unavailable WebDAV output created local files: %v", entries)
	}
}
