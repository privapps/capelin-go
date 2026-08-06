package buildconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMakefileBuildTargetsDisableCGO(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate repository root")
	}
	makefile, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	currentTarget := ""
	buildsByTarget := make(map[string]int)
	for _, line := range strings.Split(string(makefile), "\n") {
		if len(line) > 0 && line[0] != '\t' && strings.HasSuffix(line, ":") {
			currentTarget = strings.TrimSuffix(line, ":")
		}
		if strings.Contains(line, "go build") {
			buildsByTarget[currentTarget]++
			if !strings.Contains(line, "CGO_ENABLED=0") {
				t.Errorf("go build in target %q does not disable CGO: %s", currentTarget, strings.TrimSpace(line))
			}
		}
	}

	for _, target := range []string{"build", "dist"} {
		if buildsByTarget[target] == 0 {
			t.Errorf("target %q has no go build invocation", target)
		}
	}
}
