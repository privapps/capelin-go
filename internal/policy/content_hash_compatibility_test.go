package policy

import "testing"

func TestEditFileRemainsRegisteredOptInCapability(t *testing.T) {
	if !IsKnownTool(EditFile) {
		t.Fatal("edit_file is not in the registered capability vocabulary")
	}
	if !IsOptInTool(EditFile) {
		t.Fatal("edit_file is no longer an opt-in capability")
	}
	if err := ValidateAllowTool(EditFile); err != nil {
		t.Fatalf("edit_file is not accepted by --allow-tool validation: %v", err)
	}
	if DefaultAllowedTools()[EditFile] {
		t.Fatal("edit_file became enabled in the default permission set")
	}

	found := false
	for _, name := range RegisteredTools() {
		if name == EditFile {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("edit_file is missing from registered capability diagnostics")
	}
}

func TestEditFilePermissionInheritsWithoutBroadening(t *testing.T) {
	parent := DefaultAllowedTools()
	parent[EditFile] = true

	child, err := InheritChildTools(parent, nil, 1, 2)
	if err != nil {
		t.Fatalf("inherit complete edit permission: %v", err)
	}
	if !child[EditFile] {
		t.Fatal("child lost inherited edit_file permission")
	}

	restricted, err := InheritChildTools(parent, []string{EditFile}, 2, 2)
	if err != nil {
		t.Fatalf("inherit restricted edit permission at max depth: %v", err)
	}
	if !restricted[EditFile] {
		t.Fatal("child could not retain explicitly requested edit_file permission")
	}

	if _, err := InheritChildTools(DefaultAllowedTools(), []string{EditFile}, 1, 2); err == nil {
		t.Fatal("child broadened permissions by requesting edit_file from a parent without it")
	}
}
