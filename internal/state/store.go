package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"media-converter-v2/internal/domain"
)

var ErrNotFound = errors.New("job not found")

type Store struct {
	root string
	mu   sync.RWMutex
}

func (s *Store) CleanupOld(ctx context.Context, age time.Duration) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-age)
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		record, err := s.Get(ctx, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil || record.State != domain.JobCompleted {
			continue
		}
		if err := os.Remove(filepath.Join(s.root, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) Get(ctx context.Context, jobID string) (domain.JobRecord, error) {
	if !safeJobID(jobID) {
		return domain.JobRecord{}, ErrNotFound
	}
	select {
	case <-ctx.Done():
		return domain.JobRecord{}, ctx.Err()
	default:
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(s.path(jobID))
	if os.IsNotExist(err) {
		return domain.JobRecord{}, ErrNotFound
	}
	if err != nil {
		return domain.JobRecord{}, err
	}
	var record domain.JobRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return domain.JobRecord{}, err
	}
	return record, nil
}

func (s *Store) Put(ctx context.Context, record domain.JobRecord) error {
	if !safeJobID(record.JobID) {
		return errors.New("invalid job id")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.root, ".job-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path(record.JobID))
}

func (s *Store) FindByRequestHash(ctx context.Context, requestHash string) (domain.JobRecord, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return domain.JobRecord{}, err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, err := s.Get(ctx, entry.Name()[:len(entry.Name())-len(filepath.Ext(entry.Name()))])
		if err == nil && record.RequestHash == requestHash {
			return record, nil
		}
	}
	return domain.JobRecord{}, ErrNotFound
}

func (s *Store) Delete(ctx context.Context, jobID string) error {
	if !safeJobID(jobID) {
		return ErrNotFound
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(jobID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) List(ctx context.Context) ([]domain.JobRecord, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var records []domain.JobRecord
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, err := s.Get(ctx, entry.Name()[:len(entry.Name())-len(filepath.Ext(entry.Name()))])
		if err == nil {
			records = append(records, record)
		}
	}
	return records, nil
}

func RequestHash(request domain.JobRequest) string {
	data, _ := json.Marshal(request)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func (s *Store) path(jobID string) string { return filepath.Join(s.root, jobID+".json") }

func safeJobID(jobID string) bool {
	return jobID != "" && len(jobID) <= 128 && filepath.Base(jobID) == jobID && !strings.ContainsAny(jobID, "/\\\x00")
}
