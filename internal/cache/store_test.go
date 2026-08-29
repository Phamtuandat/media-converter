package cache

import (
	"context"
	"testing"
)

func TestStoreRoundTripAndRejectsUnsafeKey(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "abc123", map[string]string{"value": "ok"}); err != nil {
		t.Fatal(err)
	}
	data, err := store.Get(context.Background(), "abc123")
	if err != nil || string(data["value"]) != `"ok"` {
		t.Fatalf("unexpected cache data: %v %v", data, err)
	}
	if err := store.Put(context.Background(), "../escape", nil); err == nil {
		t.Fatal("expected unsafe key rejection")
	}
}
