package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"capelin-go/internal/output"
)

func captureOneShotStderr(t *testing.T, run func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = write
	run()
	_ = write.Close()
	os.Stderr = original
	data, err := io.ReadAll(read)
	_ = read.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestOrdinaryOneShotPersistsConversationAndEmitsResumeHint(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t, chatTurnResponse("one-shot answer", "", nil))
	var runErr error
	stderr := captureOneShotStderr(t, func() {
		runErr = testApp.app.runQuestion(context.Background(), "remember this request")
	})
	if runErr != nil {
		t.Fatalf("ordinary one-shot failed: %v", runErr)
	}
	store, err := newSessionStore(testApp.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.list()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("ordinary one-shot snapshots = %d, err=%v", len(snapshots), err)
	}
	snapshot := snapshots[0]
	if len(snapshot.Messages) != 3 || snapshot.Messages[1].Role != "user" || snapshot.Messages[1].Content != "remember this request" || snapshot.Messages[2].Content != "one-shot answer" {
		t.Fatalf("ordinary one-shot conversation was not persisted: %#v", snapshot.Messages)
	}
	wantHint := "[capelin-go] session " + snapshot.SessionUUID + "; resume with --resume " + snapshot.SessionUUID
	if !strings.Contains(stderr, wantHint) {
		t.Fatalf("resume hint = %q, want %q", stderr, wantHint)
	}
}

func TestOrdinaryOneShotFailurePersistsPartialStateAndContinuation(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t, "not-json")
	var runErr error
	stderr := captureOneShotStderr(t, func() {
		runErr = testApp.app.runQuestion(context.Background(), "recover this failed request")
	})
	if runErr == nil {
		t.Fatal("provider failure unexpectedly succeeded")
	}
	store, err := newSessionStore(testApp.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.list()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("failed one-shot snapshots = %d, err=%v", len(snapshots), err)
	}
	snapshot := snapshots[0]
	if len(snapshot.Messages) < 2 || snapshot.Messages[1].Content != "recover this failed request" {
		t.Fatalf("failed one-shot lost request context: %#v", snapshot.Messages)
	}
	if snapshot.ProviderState == nil {
		t.Fatal("failed one-shot did not retain provider continuation state")
	}
	if !strings.Contains(runErr.Error(), "invalid character") || !strings.Contains(stderr, snapshot.SessionUUID) {
		t.Fatalf("failure diagnostic or hint missing: err=%v stderr=%q", runErr, stderr)
	}
}

func TestOrdinaryOneShotCancellationPersistsRequestAndHint(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var runErr error
	stderr := captureOneShotStderr(t, func() {
		runErr = testApp.app.runQuestion(ctx, "resume after cancellation")
	})
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("cancellation error = %v", runErr)
	}
	store, err := newSessionStore(testApp.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.list()
	if err != nil || len(snapshots) != 1 || len(snapshots[0].Messages) < 2 {
		t.Fatalf("cancelled one-shot was not resumable: snapshots=%#v err=%v", snapshots, err)
	}
	if !strings.Contains(stderr, "[capelin-go] session "+snapshots[0].SessionUUID+"; resume with --resume "+snapshots[0].SessionUUID) {
		t.Fatalf("cancellation resume hint missing: %q", stderr)
	}
}

func TestOrdinaryOneShotPreservesExecutionAndPersistenceDiagnostics(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t, "not-json")
	store, err := newSessionStore(testApp.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	store.writeAtomic = func(path string, data []byte) error {
		writes++
		if writes > 1 {
			return errors.New("injected one-shot persistence failure")
		}
		return os.WriteFile(path, data, 0o600)
	}
	testApp.app.sessionStore = store

	var runErr error
	stderr := captureOneShotStderr(t, func() {
		runErr = testApp.app.runQuestion(context.Background(), "show both failures")
	})
	if runErr == nil || !strings.Contains(runErr.Error(), "invalid character") || !strings.Contains(runErr.Error(), "injected one-shot persistence failure") {
		t.Fatalf("combined diagnostic = %v", runErr)
	}
	if !strings.Contains(stderr, "[capelin-go] session ") || !strings.Contains(stderr, "; resume with --resume ") {
		t.Fatalf("persistence failure suppressed resume hint: %q", stderr)
	}
}

func TestOrdinaryOneShotFinalOnlyKeepsAnswerOnStdoutAndHintOnStderr(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t, chatTurnResponse("machine answer", "", nil))
	var stdout, sinkStderr bytes.Buffer
	testApp.app.cfg.finalOnly = true
	testApp.app.sink = output.NewFinalOnlySink(output.NewStdioSinkWithWriters(&stdout, &sinkStderr, nil), rootAgentID)
	var runErr error
	stderr := captureOneShotStderr(t, func() {
		runErr = testApp.app.runQuestion(context.Background(), "machine-readable request")
	})
	if runErr != nil {
		t.Fatalf("final-only one-shot failed: %v", runErr)
	}
	if stdout.String() != "machine answer\n" {
		t.Fatalf("final-only stdout = %q", stdout.String())
	}
	if sinkStderr.Len() != 0 {
		t.Fatalf("final-only sink stderr = %q", sinkStderr.String())
	}
	if !strings.Contains(stderr, "[capelin-go] session ") || !strings.Contains(stderr, "; resume with --resume ") {
		t.Fatalf("final-only hint missing from stderr: %q", stderr)
	}
}

func TestOrdinaryOneShotUsesConfiguredOverallTimeout(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.cfg.modelRequestTimeout = 20 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"late"}}]}`))
	}))
	defer server.Close()
	testApp.app.client.endpoint = server.URL
	testApp.app.client.http = server.Client()

	started := time.Now()
	err := testApp.app.runQuestion(context.Background(), "bounded request")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("one-shot timeout error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("one-shot timeout took %s, want less than one second", elapsed)
	}
}

func TestOneShotGoalUsesConfiguredOverallTimeout(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.cfg.yolo = true
	testApp.app.cfg.modelRequestTimeout = 20 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"late"}}]}`))
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	testApp.app.client.endpoint = server.URL
	testApp.app.client.http = server.Client()

	started := time.Now()
	err := testApp.app.runQuestion(context.Background(), "/goal bounded goal")
	if err == nil || !strings.Contains(err.Error(), "one-shot goal incomplete") {
		t.Fatalf("one-shot goal timeout error = %v, want incomplete goal error", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("one-shot goal timeout took %s, want less than 250ms", elapsed)
	}
}
