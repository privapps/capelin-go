package subagents

import (
	"context"
	"testing"
)

func TestManagerRunsChildThroughNarrowRunnerSeam(t *testing.T) {
	manager := New(Config{MaxDepth: 1, MaxChildren: 2, MaxParallel: 1, DefaultTimeoutSec: 2, MaxTimeoutSec: 2}, func(_ context.Context, runtime *Runtime, session *Session) (string, error) {
		if runtime.Depth != 1 || session.ParentID != "root" {
			t.Fatalf("runtime/session = %#v/%#v", runtime, session)
		}
		return "completed output", nil
	})
	root := &Runtime{SessionID: "root", Role: "coordinator", AllowedTools: map[string]bool{"web_search": true}}
	created, err := manager.Create(context.Background(), root, CreateArgs{Question: "inspect", ExecutionMode: "sequential"})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := manager.Run(context.Background(), root, RunArgs{ID: created.ID, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(completed.Status) != "completed" || completed.Output != "completed output" {
		t.Fatalf("completed session = %#v", completed)
	}
	visible, err := manager.List(root, ListArgs{})
	if err != nil || len(visible) != 1 {
		t.Fatalf("visible sessions = %#v, err=%v", visible, err)
	}
}
