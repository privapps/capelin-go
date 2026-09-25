package providers

import (
	"reflect"
	"testing"

	"capelin-go/internal/contracts"
)

func TestZenEditFileMappingPreservesHashContract(t *testing.T) {
	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":         map[string]any{"type": "string"},
			"old_str":      map[string]any{"type": "string"},
			"new_str":      map[string]any{"type": "string"},
			"content_hash": map[string]any{"type": "string"},
		},
		"required":             []any{"path", "old_str", "new_str", "content_hash"},
		"additionalProperties": false,
	}

	mapped, responseTools, err := zenTools([]contracts.Tool{{
		Type: "function",
		Function: contracts.ToolSpec{
			Name:        "edit_file",
			Description: "Replace an exact string using a compact or legacy full-file content hash.",
			Parameters:  parameters,
		},
	}}, false, nil)
	if err != nil {
		t.Fatalf("map edit_file for Zen: %v", err)
	}
	if len(mapped) != 1 {
		t.Fatalf("mapped tools = %#v, want one tool", mapped)
	}
	if got := mapped[0].Function.Name; got != "edit" {
		t.Fatalf("Zen wire name = %q, want edit", got)
	}
	if got := responseTools["edit"]; got != "edit_file" {
		t.Fatalf("Zen response mapping = %q, want edit_file", got)
	}
	if !reflect.DeepEqual(mapped[0].Function.Parameters, parameters) {
		t.Fatalf("Zen mapping changed edit_file parameters: got %#v, want %#v", mapped[0].Function.Parameters, parameters)
	}
}
