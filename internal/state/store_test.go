package state

import (
	"context"
	"testing"

	"media-converter-v2/internal/domain"
)

func TestStorePersistsAndListsJobs(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := domain.JobRecord{JobID: "job-1", RequestHash: "hash", State: domain.JobQueued}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), record.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestHash != record.RequestHash || got.State != record.State {
		t.Fatalf("persisted record mismatch: %+v", got)
	}
	if _, err := store.FindByRequestHash(context.Background(), "hash"); err != nil {
		t.Fatal(err)
	}
	record.State = domain.JobCompleted
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	records, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State != domain.JobCompleted {
		t.Fatalf("unexpected records: %+v", records)
	}
}
