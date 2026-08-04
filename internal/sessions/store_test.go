package sessions

import (
	"capelin-go/internal/contracts"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreUsesTemporaryWorkspaceAndResolvesLegacySnapshots(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	created, err := store.Create([]contracts.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "remember this"}})
	if err != nil {
		t.Fatal(err)
	}
	if !IsUUID(created.SessionUUID) || created.Topic != "remember this" || created.LastInput != "remember this" {
		t.Fatalf("unexpected created snapshot: %#v", created)
	}

	legacyID := "00000000-0000-4000-8000-000000000001"
	legacy := `{"session_id":"` + legacyID + `","created_at":"2026-08-04T11:00:00Z","updated_at":"2026-08-04T11:00:00Z","last_response":"old answer","messages":[{"role":"system","content":"s"},{"role":"user","content":"old prompt"}]}`
	if err := os.MkdirAll(store.Directory(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Directory(), legacyID+".json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Resolve(legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SessionUUID != legacyID || loaded.LastContent != "old answer" || loaded.Topic != "old prompt" || len(loaded.Todos) != 0 {
		t.Fatalf("legacy snapshot did not normalize: %#v", loaded)
	}
	listed, err := store.List()
	if err != nil || len(listed) != 2 {
		t.Fatalf("list got %d snapshots, err=%v", len(listed), err)
	}
}

func TestStoreFailedAtomicReplacementPreservesPreviousSnapshot(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Create([]contracts.Message{{Role: "system", Content: "before"}})
	if err != nil {
		t.Fatal(err)
	}
	store.SetAtomicWriter(func(string, []byte) error { return errors.New("injected failure") })
	snapshot.Messages = append(snapshot.Messages, contracts.Message{Role: "user", Content: "after"})
	if err := store.Save(snapshot); err == nil {
		t.Fatal("injected save unexpectedly succeeded")
	}
	store.SetAtomicWriter(func(path string, data []byte) error { return os.WriteFile(path, data, 0o600) })
	loaded, err := store.Resolve(snapshot.SessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content != "before" {
		t.Fatalf("previous snapshot was not preserved: %#v", loaded.Messages)
	}
}

func TestSelectorsRejectPathsAndAmbiguousPrefixes(t *testing.T) {
	if IsSelector("../") || IsSelector(strings.Repeat("a", 37)) {
		t.Fatal("unsafe selector accepted")
	}
	if IsUUID("../00000000-0000-4000-8000-000000000000") {
		t.Fatal("unsafe UUID accepted")
	}
}
