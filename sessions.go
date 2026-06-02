package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sessionSnapshot captures the full state of a top-level agent conversation
// so that it can be resumed in a later TUI session via /attach.
type sessionSnapshot struct {
	SessionUUID  string       `json:"sessionUUID"`
	Name         string       `json:"name,omitempty"` // short LLM-generated title
	CreatedAt    time.Time    `json:"createdAt"`
	UpdatedAt    time.Time    `json:"updatedAt"`
	LastContent  string       `json:"lastContent"`  // first 100 chars of last assistant message
	MessageCount int          `json:"messageCount"`
	Messages     []apiMessage `json:"messages"`
}

// sessionsDir returns the directory where session snapshots are stored.
func sessionsDir(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".capelin-go", "sessions")
}

// saveSessionMessages writes (or overwrites) a session snapshot file for the
// given UUID. It is safe to call from goroutines — each UUID gets its own file.
func saveSessionMessages(workspaceRoot, sessionUUID, agentName string, msgs []apiMessage, createdAt time.Time) error {
	dir := sessionsDir(workspaceRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Extract the last assistant text for preview.
	lastContent := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
			c := []rune(strings.TrimSpace(msgs[i].Content))
			if len(c) > 100 {
				c = c[:100]
			}
			lastContent = string(c)
			break
		}
	}

	snap := sessionSnapshot{
		SessionUUID:  sessionUUID,
		Name:         agentName,
		CreatedAt:    createdAt,
		UpdatedAt:    time.Now().UTC(),
		LastContent:  lastContent,
		MessageCount: len(msgs),
		Messages:     msgs,
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, sessionUUID+".json")
	return os.WriteFile(path, raw, 0o644)
}

// listSessionSnapshots reads all saved session snapshots from the sessions
// directory and returns them sorted newest-updated-first. The Messages field
// is populated for each snapshot.
func listSessionSnapshots(workspaceRoot string) ([]sessionSnapshot, error) {
	dir := sessionsDir(workspaceRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var snapshots []sessionSnapshot
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var snap sessionSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			continue
		}
		snapshots = append(snapshots, snap)
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].UpdatedAt.After(snapshots[j].UpdatedAt)
	})
	return snapshots, nil
}

// loadSessionMessages loads the messages for a single session UUID.
func loadSessionMessages(workspaceRoot, sessionUUID string) ([]apiMessage, error) {
	path := filepath.Join(sessionsDir(workspaceRoot), sessionUUID+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap sessionSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	return snap.Messages, nil
}
