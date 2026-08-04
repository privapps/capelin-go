package sessions

import (
	"encoding/json"
	"reflect"
	"testing"

	"capelin-go/internal/contracts"
)

func TestSnapshotRoundTripsProviderStateAndLegacySnapshotsRemainValid(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := &contracts.ContinuationState{Provider: "responses", Version: 1, Data: json.RawMessage(`{"input":[{"type":"reasoning"}]}`)}
	snapshot := Snapshot{
		SessionUUID:   "123e4567-e89b-12d3-a456-426614174000",
		Messages:      []contracts.Message{{Role: "assistant", ReasoningContent: stringPointer("thought")}},
		ProviderState: state,
	}
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Resolve(snapshot.SessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	var gotData, wantData any
	_ = json.Unmarshal(loaded.ProviderState.Data, &gotData)
	_ = json.Unmarshal(state.Data, &wantData)
	if loaded.ProviderState == nil || loaded.ProviderState.Provider != state.Provider || !reflect.DeepEqual(gotData, wantData) {
		t.Fatalf("provider state=%#v, want %#v", loaded.ProviderState, state)
	}
	if loaded.Messages[0].ReasoningContent == nil || *loaded.Messages[0].ReasoningContent != "thought" {
		t.Fatalf("normalized reasoning=%#v", loaded.Messages[0].ReasoningContent)
	}

	legacy := `{"session_id":"123e4567-e89b-12d3-a456-426614174001","messages":[{"role":"user","content":"old"}]}`
	var old Snapshot
	if err := json.Unmarshal([]byte(legacy), &old); err != nil {
		t.Fatal(err)
	}
	if old.ProviderState != nil || old.SessionUUID == "" || len(old.Messages) != 1 {
		t.Fatalf("legacy snapshot=%#v", old)
	}
}

func stringPointer(value string) *string { return &value }
