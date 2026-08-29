package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"media-converter-v2/internal/domain"
)

type Artifact struct {
	ID     string
	Path   string
	Size   int64
	SHA256 string
	File   *os.File
}

type LocalStore struct {
	inputRoot        string
	outputRoot       string
	legacyOutputRoot string
	outputMode       string
}

const (
	OutputModeLocal  = "local"
	OutputModeWebDAV = "webdav"
)

func (s *LocalStore) CleanupOld(ctx context.Context, age time.Duration) error {
	return s.CleanupOldExcept(ctx, age, nil)
}

func (s *LocalStore) CleanupOldExcept(ctx context.Context, age time.Duration, keep map[string]struct{}) error {
	if err := s.checkOutputMounted(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.outputRoot)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-age)
	for _, entry := range entries {
		if entry.Name() == ".tmp" {
			if err := cleanupDirectory(filepath.Join(s.outputRoot, entry.Name()), cutoff); err != nil {
				return err
			}
			continue
		}
		id := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if _, ok := keep[id]; ok {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.outputRoot, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func cleanupDirectory(root string, cutoff time.Time) error {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func NewLocalStore(inputRoot, outputRoot string) (*LocalStore, error) {
	return NewLocalStoreWithMode(inputRoot, outputRoot, OutputModeLocal)
}

func NewLocalStoreWithMode(inputRoot, outputRoot, outputMode string) (*LocalStore, error) {
	return NewLocalStoreWithModeAndLegacy(inputRoot, outputRoot, outputMode, "")
}

// NewLocalStoreWithModeAndLegacy keeps the new output root writable while
// allowing rollback-era artifacts to remain readable from a separate root.
// The legacy root is never created or written by this store.
func NewLocalStoreWithModeAndLegacy(inputRoot, outputRoot, outputMode, legacyOutputRoot string) (*LocalStore, error) {
	if outputMode == "" {
		outputMode = OutputModeLocal
	}
	if outputMode != OutputModeLocal && outputMode != OutputModeWebDAV {
		return nil, fmt.Errorf("unsupported output mode %q", outputMode)
	}
	if err := os.MkdirAll(inputRoot, 0o700); err != nil {
		return nil, err
	}
	if legacyOutputRoot != "" {
		inputAbs, err := filepath.Abs(inputRoot)
		if err != nil {
			return nil, err
		}
		outputAbs, err := filepath.Abs(outputRoot)
		if err != nil {
			return nil, err
		}
		legacyAbs, err := filepath.Abs(legacyOutputRoot)
		if err != nil {
			return nil, err
		}
		if pathsOverlap(legacyAbs, inputAbs) || pathsOverlap(legacyAbs, outputAbs) {
			return nil, fmt.Errorf("legacy artifact root must be separate from service roots")
		}
		legacyOutputRoot = legacyAbs
	}
	// Never create a local directory under an unavailable WebDAV mount. The
	// service remains not-ready until the mount is present.
	if outputMode == OutputModeLocal {
		if err := os.MkdirAll(outputRoot, 0o700); err != nil {
			return nil, err
		}
	} else if isMountedPath(outputRoot) {
		if err := os.MkdirAll(outputRoot, 0o700); err != nil {
			return nil, err
		}
	}
	return &LocalStore{inputRoot: inputRoot, outputRoot: outputRoot, legacyOutputRoot: legacyOutputRoot, outputMode: outputMode}, nil
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

func (s *LocalStore) OutputReady(ctx context.Context) error {
	if err := s.checkOutputMounted(ctx); err != nil {
		return err
	}
	return checkWritableDirectory(s.outputRoot)
}

func (s *LocalStore) CheckOutputMounted(ctx context.Context) error {
	return s.checkOutputMounted(ctx)
}

func (s *LocalStore) checkOutputMounted(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if s.outputMode == OutputModeWebDAV && !isMountedPath(s.outputRoot) {
		return domain.NewError("storage_not_mounted", "WebDAV output mount is unavailable", "storage", true, os.ErrNotExist)
	}
	return nil
}

func (s *LocalStore) OpenInput(ctx context.Context, id string) (Artifact, error) {
	select {
	case <-ctx.Done():
		return Artifact{}, ctx.Err()
	default:
	}
	path, err := safeJoin(s.inputRoot, id)
	if err != nil {
		return Artifact{}, domain.NewError("invalid_artifact_id", "invalid artifact identity", "resolve", false, err)
	}
	f, err := openNoFollow(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Artifact{}, domain.NewError("artifact_not_found", "input artifact does not exist", "resolve", false, err)
		}
		if errors.Is(err, syscall.ELOOP) {
			return Artifact{}, domain.NewError("input_not_file", "input artifact is a symlink", "resolve", false, err)
		}
		return Artifact{}, domain.NewError("storage_read_failed", "could not open input artifact", "resolve", true, err)
	}
	openedInfo, err := f.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() {
		_ = f.Close()
		return Artifact{}, domain.NewError("input_not_file", "input artifact is not a regular file", "resolve", false, err)
	}
	return Artifact{ID: id, Path: path, Size: openedInfo.Size(), File: f}, nil
}

// Stage stores an uploaded artifact under an internally generated ID. The
// temporary file remains inside the input root so the final rename is atomic.
func (s *LocalStore) Stage(ctx context.Context, in io.Reader, limit int64) (Artifact, error) {
	if in == nil {
		return Artifact{}, domain.NewError("request_invalid", "artifact body is required", "upload", false, nil)
	}
	select {
	case <-ctx.Done():
		return Artifact{}, ctx.Err()
	default:
	}
	if limit <= 0 {
		return Artifact{}, domain.NewError("request_invalid", "artifact size limit must be positive", "upload", false, nil)
	}
	tmpDir := filepath.Join(s.inputRoot, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not create staging workspace", "upload", true, err)
	}
	tmp, err := os.CreateTemp(tmpDir, ".staging-*")
	if err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not create staging artifact", "upload", true, err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	hash := sha256.New()
	n, err := Copy(ctx, io.MultiWriter(tmp, hash), io.LimitReader(in, limit+1))
	if err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not stage artifact", "upload", true, err)
	}
	if n == 0 {
		return Artifact{}, domain.NewError("request_invalid", "artifact body must not be empty", "upload", false, nil)
	}
	if n > limit {
		return Artifact{}, domain.NewError("input_size_exceeded", "artifact exceeds configured size limit", "upload", false, nil)
	}
	if err := tmp.Sync(); err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not flush staged artifact", "upload", true, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not secure staged artifact", "upload", true, err)
	}
	if err := tmp.Close(); err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not close staged artifact", "upload", true, err)
	}
	id := NewArtifactID("stg")
	final := filepath.Join(s.inputRoot, id)
	if err := os.Rename(tmpName, final); err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not commit staged artifact", "upload", true, err)
	}
	keep = true
	return Artifact{ID: id, Path: final, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (s *LocalStore) Begin(ctx context.Context, id, ext string) (string, string, error) {
	if err := s.checkOutputMounted(ctx); err != nil {
		return "", "", err
	}
	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	default:
	}
	if id == "" {
		id = newID()
	}
	if ext == "" {
		ext = ".bin"
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	tmpDir := filepath.Join(s.outputRoot, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return "", "", domain.NewError("storage_write_failed", "could not create output workspace", "commit", true, err)
	}
	tmp, err := os.CreateTemp(tmpDir, ".artifact-*")
	if err != nil {
		return "", "", domain.NewError("storage_write_failed", "could not create temporary output", "commit", true, err)
	}
	if err := tmp.Close(); err != nil {
		return "", "", domain.NewError("storage_write_failed", "could not initialize temporary output", "commit", true, err)
	}
	path := filepath.Join(s.outputRoot, id+ext)
	return path, tmp.Name(), nil
}

func NewArtifactID(prefix string) string { return prefix + "-" + newID() }

func (s *LocalStore) Commit(ctx context.Context, tmp, final string) (Artifact, error) {
	if err := s.checkOutputMounted(ctx); err != nil {
		return Artifact{}, err
	}
	select {
	case <-ctx.Done():
		return Artifact{}, ctx.Err()
	default:
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not secure temporary output", "commit", true, err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not create artifact directory", "commit", true, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not commit artifact", "commit", true, err)
	}
	info, err := os.Stat(final)
	if err != nil {
		return Artifact{}, domain.NewError("storage_write_failed", "could not inspect committed artifact", "commit", true, err)
	}
	return Artifact{ID: strings.TrimSuffix(filepath.Base(final), filepath.Ext(final)), Path: final, Size: info.Size()}, nil
}

func (s *LocalStore) Remove(ctx context.Context, id string) error {
	if err := s.checkOutputMounted(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if !safeArtifactID(id) {
		return domain.NewError("invalid_artifact_id", "invalid artifact identity", "artifact", false, os.ErrNotExist)
	}
	entries, err := os.ReadDir(s.outputRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), id+".") {
			if err := os.Remove(filepath.Join(s.outputRoot, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// RemoveInput deletes a staged upload after the caller has finished using it.
func (s *LocalStore) RemoveInput(ctx context.Context, id string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	path, err := safeJoin(s.inputRoot, id)
	if err != nil || !safeArtifactID(id) {
		return domain.NewError("invalid_artifact_id", "invalid artifact identity", "artifact", false, os.ErrNotExist)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *LocalStore) OpenCommitted(ctx context.Context, id string) (Artifact, error) {
	select {
	case <-ctx.Done():
		return Artifact{}, ctx.Err()
	default:
	}
	if !safeArtifactID(id) {
		return Artifact{}, domain.NewError("invalid_artifact_id", "invalid artifact identity", "artifact", false, os.ErrNotExist)
	}
	outputMountErr := s.checkOutputMounted(ctx)
	if outputMountErr == nil {
		artifact, found, err := s.openCommittedFromRoot(id, s.outputRoot)
		if err != nil {
			return Artifact{}, err
		}
		if found {
			return artifact, nil
		}
	}
	if s.legacyOutputRoot != "" {
		artifact, found, err := s.openCommittedFromRoot(id, s.legacyOutputRoot)
		if err != nil {
			return Artifact{}, err
		}
		if found {
			return artifact, nil
		}
	}
	if outputMountErr != nil {
		return Artifact{}, outputMountErr
	}
	return Artifact{}, domain.NewError("artifact_not_found", "output artifact does not exist", "artifact", false, os.ErrNotExist)
}

func (s *LocalStore) openCommittedFromRoot(id, root string) (Artifact, bool, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return Artifact{}, false, nil
	}
	if err != nil {
		return Artifact{}, false, domain.NewError("storage_read_failed", "could not inspect output artifacts", "artifact", true, err)
	}
	var matches []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, id+".") {
			matches = append(matches, filepath.Join(root, name))
		}
	}
	if len(matches) == 0 {
		return Artifact{}, false, nil
	}
	if len(matches) > 1 {
		return Artifact{}, false, domain.NewError("internal_error", "artifact identity is ambiguous", "artifact", false, nil)
	}
	f, err := openNoFollow(matches[0])
	if err != nil {
		return Artifact{}, false, domain.NewError("storage_read_failed", "could not open output artifact", "artifact", true, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return Artifact{}, false, domain.NewError("storage_read_failed", "could not inspect output artifact", "artifact", true, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return Artifact{}, false, domain.NewError("input_not_file", "output artifact is not a regular file", "artifact", false, nil)
	}
	return Artifact{ID: id, Path: matches[0], Size: info.Size(), File: f}, true, nil
}

func (s *LocalStore) CleanupStagedOldExcept(ctx context.Context, age time.Duration, keep map[string]struct{}) error {
	if err := cleanupDirectory(filepath.Join(s.inputRoot, ".tmp"), time.Now().Add(-age)); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.inputRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cutoff := time.Now().Add(-age)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "stg-") {
			continue
		}
		if _, ok := keep[entry.Name()]; ok {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.inputRoot, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func openNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func Copy(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	var total int64
	buf := make([]byte, 128*1024)
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, err := dst.Write(buf[:n])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func (s *LocalStore) CopyOutput(ctx context.Context, src, dst string, limit int64) error {
	if err := s.checkOutputMounted(ctx); err != nil {
		return err
	}
	if limit <= 0 {
		return domain.NewError("request_invalid", "output size limit must be positive", "commit", false, nil)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	n, err := Copy(ctx, out, io.LimitReader(in, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return domain.NewError("output_size_exceeded", "output exceeds configured size limit", "commit", false, nil)
	}
	if err := s.checkOutputMounted(ctx); err != nil {
		return err
	}
	return nil
}

func checkWritableDirectory(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".readiness-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func safeJoin(root, id string) (string, error) {
	if id == "" || filepath.IsAbs(id) || filepath.Base(id) != id || id == "." || id == ".." || strings.ContainsAny(id, "/\\\x00") {
		return "", fmt.Errorf("invalid artifact id")
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(cleanRoot, id)
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if filepath.Dir(cleanPath) != cleanRoot {
		return "", fmt.Errorf("artifact escapes root")
	}
	return cleanPath, nil
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("artifact-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func safeArtifactID(id string) bool {
	if id == "" || filepath.Base(id) != id {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
