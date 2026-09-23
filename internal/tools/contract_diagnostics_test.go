package tools

import (
	"strings"
	"testing"
)

func TestExecuteProgramContractDescribesDirectInvocation(t *testing.T) {
	var description string
	for _, tool := range Build(map[string]bool{ExecuteProgram: true}) {
		if tool.Function.Name == ExecuteProgram {
			description = tool.Function.Description
			break
		}
	}
	if description == "" {
		t.Fatal("execute_program was missing from the enabled tool catalog")
	}
	for _, want := range []string{"directly", "executable", "argument vector", "never invoke or parse a shell"} {
		if !strings.Contains(description, want) {
			t.Fatalf("execute_program description %q does not contain %q", description, want)
		}
	}
}
