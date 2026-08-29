package cache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrNotFound = errors.New("cache entry not found")

type Store struct{ root string }

func (s *Store) ReferencedArtifacts(ctx context.Context) (map[string]struct{}, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]struct{})
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, entry.Name()))
		if err != nil {
			continue
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(data, &envelope) != nil {
			continue
		}
		var result struct {
			Output struct {
				ArtifactID string `json:"artifact_id"`
			} `json:"output"`
			Thumbnail struct {
				ArtifactID string `json:"artifact_id"`
			} `json:"thumbnail"`
		}
		raw, ok := envelope["result"]
		if !ok || json.Unmarshal(raw, &result) != nil {
			continue
		}
		if result.Output.ArtifactID != "" {
			refs[result.Output.ArtifactID] = struct{}{}
		}
		if result.Thumbnail.ArtifactID != "" {
			refs[result.Thumbnail.ArtifactID] = struct{}{}
		}
	}
	return refs, nil
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

func (s *Store) Get(ctx context.Context, key string) (result map[string]json.RawMessage, err error) {
	if !safeKey(key) {
		return nil, ErrNotFound
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	data, err := os.ReadFile(filepath.Join(s.root, key+".json"))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) Put(ctx context.Context, key string, value any) error {
	if !safeKey(key) {
		return ErrNotFound
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.root, ".cache-*")
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
	return os.Rename(tmpName, filepath.Join(s.root, key+".json"))
}

func safeKey(key string) bool {
	if key == "" || filepath.Base(key) != key {
		return false
	}
	return strings.Trim(key, "0123456789abcdef") == ""
}
