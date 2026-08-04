package app

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProductionAppCodeUsesContractsInsteadOfCompatibilityTypes(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate app source directory")
	}
	dir := filepath.Dir(sourceFile)
	entries, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, importSpec := range file.Imports {
			if strings.Trim(importSpec.Path.Value, "\"") == "capelin-go/internal/types" {
				t.Errorf("production file %s imports compatibility package internal/types; use internal/contracts", filepath.Base(path))
			}
		}
	}
}
