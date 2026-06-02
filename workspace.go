package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// workspaceEntry records a single top-level agent that belongs to a workspace.
type workspaceEntry struct {
	SessionUUID string `json:"sessionUUID"`
	Name        string `json:"name,omitempty"`
}

// workspaceSnapshot is the on-disk representation of a workspace.
type workspaceSnapshot struct {
	Name    string           `json:"name"`
	SavedAt time.Time        `json:"savedAt"`
	Agents  []workspaceEntry `json:"agents"`
}

// workspaceDir returns the directory where workspace files are stored.
func workspaceDir(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".capelin-go", "workspace")
}

// saveWorkspace writes a workspace snapshot to .capelin-go/workspace/<name>.json.
func saveWorkspace(workspaceRoot, name string, agents []workspaceEntry) error {
	dir := workspaceDir(workspaceRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	snap := workspaceSnapshot{
		Name:    name,
		SavedAt: time.Now().UTC(),
		Agents:  agents,
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o644)
}

// loadWorkspace reads a workspace snapshot by name.
func loadWorkspace(workspaceRoot, name string) (*workspaceSnapshot, error) {
	path := filepath.Join(workspaceDir(workspaceRoot), name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap workspaceSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// listWorkspaces returns the names of all saved workspaces, excluding "last".
// Names are sorted alphabetically.
func listWorkspaces(workspaceRoot string) ([]string, error) {
	dir := workspaceDir(workspaceRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if name == "last" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
