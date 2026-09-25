package tools

import "testing"

func TestEditFileCapabilityKeepsCanonicalLocalName(t *testing.T) {
	catalog := Build(map[string]bool{EditFile: true})
	if len(catalog) != 1 {
		t.Fatalf("edit_file catalog = %#v, want one tool", catalog)
	}
	if got := catalog[0].Function.Name; got != EditFile {
		t.Fatalf("local edit tool name = %q, want %q", got, EditFile)
	}
}
