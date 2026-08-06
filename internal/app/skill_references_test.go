package app

import (
	"capelin-go/internal/skills"
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func testSkill(name, content string) skills.Skill {
	return skills.Skill{Name: name, Content: content}
}

func TestPrepareSkillPromptResolvesExactReferencesAndPreservesUnknownTokens(t *testing.T) {
	available := map[string]skills.Skill{
		"research":  testSkill("research", "research guidance"),
		"graphify":  testSkill("graphify", "graph guidance"),
		"bad name":  testSkill("bad name", "must not resolve"),
		"HOME-tool": testSkill("HOME-tool", "home guidance"),
		"1":         testSkill("1", "must remain positional text"),
	}

	prepared, newlyLoaded := prepareSkillPrompt(
		`$research compare $graphify, then $research; keep $HOME $1 $5.00 $missing \$research`,
		available,
		nil,
	)

	if !reflect.DeepEqual(newlyLoaded, []string{"research", "graphify"}) {
		t.Fatalf("newly loaded skills = %#v, want research then graphify", newlyLoaded)
	}
	for _, want := range []string{
		"[CAPELIN SELECTED SKILL: research]",
		"[CAPELIN SELECTED SKILL: graphify]",
		"research guidance",
		"graph guidance",
		"User request:",
		"$HOME $1 $5.00 $missing $research",
	} {
		if !strings.Contains(prepared, want) {
			t.Fatalf("prepared prompt does not contain %q:\n%s", want, prepared)
		}
	}
	if strings.Count(prepared, "research guidance") != 1 || strings.Count(prepared, "graph guidance") != 1 {
		t.Fatalf("duplicate skill content was injected:\n%s", prepared)
	}
	if strings.Contains(prepared, "bad name") {
		t.Fatal("invalid skill name was treated as a reference")
	}
	if strings.Contains(prepared, "must remain positional text") {
		t.Fatal("numeric-only positional token was treated as a reference")
	}
}

func TestPrepareSkillPromptReferenceOnlyInputGetsUsefulTask(t *testing.T) {
	prepared, newlyLoaded := prepareSkillPrompt("$research", map[string]skills.Skill{
		"research": testSkill("research", "guidance"),
	}, nil)
	if len(newlyLoaded) != 1 || newlyLoaded[0] != "research" {
		t.Fatalf("unexpected selected skills: %#v", newlyLoaded)
	}
	if !strings.Contains(prepared, "Please apply the selected skill guidance") {
		t.Fatalf("reference-only request did not get a useful task:\n%s", prepared)
	}
}

func TestPrepareSkillPromptPreservesInternalWhitespaceAfterReferenceRemoval(t *testing.T) {
	cleaned, references, _ := resolveSkillReferences("before  $research  \nline\n\n  after", map[string]skills.Skill{
		"research": testSkill("research", "guidance"),
	})
	if !reflect.DeepEqual(references, []string{"research"}) {
		t.Fatalf("unexpected references: %#v", references)
	}
	if cleaned != "before    \nline\n\n  after" {
		t.Fatalf("internal whitespace changed: %q", cleaned)
	}
}

func TestPrepareSkillPromptDoesNotChangeInputWithoutReferences(t *testing.T) {
	input := "  keep $HOME and $1 exactly as written  "
	prepared, newlyLoaded := prepareSkillPrompt(input, map[string]skills.Skill{
		"research": testSkill("research", "guidance"),
	}, nil)
	if prepared != input {
		t.Fatalf("input changed without a selected skill: got %q, want %q", prepared, input)
	}
	if newlyLoaded != nil {
		t.Fatalf("unexpected loaded skills: %#v", newlyLoaded)
	}
}

func TestPrepareSkillPromptUsesMarkerForAlreadyLoadedSkill(t *testing.T) {
	prepared, newlyLoaded := prepareSkillPrompt(
		"please use $research again",
		map[string]skills.Skill{"research": testSkill("research", "full guidance")},
		map[string]bool{"research": true},
	)
	if newlyLoaded != nil {
		t.Fatalf("already loaded skill returned as newly loaded: %#v", newlyLoaded)
	}
	if strings.Contains(prepared, "full guidance") {
		t.Fatalf("already loaded skill content was repeated:\n%s", prepared)
	}
	if !strings.Contains(prepared, "research already loaded") || !strings.Contains(prepared, "please use ") || !strings.Contains(prepared, "again") {
		t.Fatalf("already-loaded marker or cleaned request missing:\n%s", prepared)
	}
}

func TestPrepareSkillPromptEnforcesPerSkillAndAggregateLimits(t *testing.T) {
	content := strings.Repeat("x", maxSkillContent+100)
	available := map[string]skills.Skill{
		"one":   testSkill("one", content),
		"two":   testSkill("two", content),
		"three": testSkill("three", content),
	}
	prepared, newlyLoaded := prepareSkillPrompt("$one $two $three", available, nil)

	if !reflect.DeepEqual(newlyLoaded, []string{"one", "two", "three"}) {
		t.Fatalf("unexpected loaded order: %#v", newlyLoaded)
	}
	if !strings.Contains(prepared, strings.TrimSpace(selectedSkillContentTruncationMarker)) {
		t.Fatalf("expected per-skill truncation for the first skill:\n%s", prepared)
	}
	if !strings.Contains(prepared, selectedSkillAggregateTruncationMarker) {
		t.Fatal("aggregate truncation marker missing")
	}
	if !strings.Contains(prepared, "read_skill") {
		t.Fatal("aggregate truncation did not tell the model how to obtain complete content")
	}
}

func TestInteractiveCompleterHandlesInlineSkillsAndSlashCommands(t *testing.T) {
	completer := interactiveCommandCompleter(map[string]skills.Skill{
		"research":    testSkill("research", ""),
		"graphify":    testSkill("graphify", ""),
		"not a token": testSkill("not a token", ""),
	})

	candidates, offset := completer.Do([]rune("use $"), len([]rune("use $")))
	if offset != 1 || !reflect.DeepEqual(runeStrings(candidates), []string{"graphify ", "research "}) {
		t.Fatalf("inline skill completion mismatch: candidates=%q offset=%d", candidates, offset)
	}

	candidates, offset = completer.Do([]rune("use $re here"), len([]rune("use $re")))
	if offset != 3 || !reflect.DeepEqual(runeStrings(candidates), []string{"search"}) {
		t.Fatalf("inline partial completion mismatch: candidates=%q offset=%d", candidates, offset)
	}

	candidates, offset = completer.Do([]rune("/ex"), 3)
	if offset != 3 || !reflect.DeepEqual(runeStrings(candidates), []string{"it "}) {
		t.Fatalf("slash completion regressed: candidates=%q offset=%d", candidates, offset)
	}
}

func TestInteractiveSkillLoadIsCommittedOnlyAfterSuccessfulTurn(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		testApp := newInteractiveTurnTestApp(t)
		testApp.app.skills = map[string]skills.Skill{"research": testSkill("research", "selected guidance")}
		session := &interactiveSession{
			messages: []types.Message{{Role: "system", Content: "test"}},
			runtime:  testApp.app.rootRuntime(),
		}
		if testApp.app.runInteractiveTurn(context.Background(), session, "$research do the task") {
			t.Fatal("successful skill turn stopped the session")
		}
		if !session.loadedSkills["research"] {
			t.Fatal("successful turn did not commit selected skill")
		}
		request := testApp.requests[0]
		if !strings.Contains(request.Messages[len(request.Messages)-1].Content, "selected guidance") {
			t.Fatalf("selected skill content missing from model request: %#v", request.Messages)
		}
	})

	t.Run("failed retry gets full context", func(t *testing.T) {
		testApp := newInteractiveTurnTestAppWithResponses(t, `{`, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		testApp.app.skills = map[string]skills.Skill{"research": testSkill("research", "selected guidance")}
		session := &interactiveSession{
			messages: []types.Message{{Role: "system", Content: "test"}},
			runtime:  testApp.app.rootRuntime(),
		}
		if testApp.app.runInteractiveTurn(context.Background(), session, "$research first attempt") {
			t.Fatal("failed skill turn stopped the session")
		}
		if session.loadedSkills["research"] {
			t.Fatal("failed turn committed selected skill")
		}
		if testApp.app.runInteractiveTurn(context.Background(), session, "$research retry") {
			t.Fatal("retry skill turn stopped the session")
		}
		if !session.loadedSkills["research"] {
			t.Fatal("successful retry did not commit selected skill")
		}
		if len(testApp.requests) != 2 || !strings.Contains(testApp.requests[1].Messages[len(testApp.requests[1].Messages)-1].Content, "selected guidance") {
			t.Fatalf("retry did not receive full skill context: %#v", testApp.requests)
		}
	})
}

func TestInteractiveRepeatedSkillReferenceUsesAlreadyLoadedMarker(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		`{"choices":[{"message":{"role":"assistant","content":"first"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"second"}}]}`,
	)
	testApp.app.skills = map[string]skills.Skill{"research": testSkill("research", "selected guidance")}
	session := &interactiveSession{messages: []types.Message{{Role: "system", Content: "test"}}, runtime: testApp.app.rootRuntime()}
	if testApp.app.runInteractiveTurn(context.Background(), session, "$research first") || testApp.app.runInteractiveTurn(context.Background(), session, "$research second") {
		t.Fatal("interactive skill turn unexpectedly stopped")
	}
	if len(testApp.requests) != 2 {
		t.Fatalf("model request count = %d, want 2", len(testApp.requests))
	}
	second := testApp.requests[1].Messages[len(testApp.requests[1].Messages)-1].Content
	if strings.Contains(second, "selected guidance") || !strings.Contains(second, "research already loaded") {
		t.Fatalf("repeated skill context was not compacted:\n%s", second)
	}
}

func TestOneShotSkillReferencePreparesTheUserRequest(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.cfg.finalOnly = true
	testApp.app.skills = map[string]skills.Skill{"research": testSkill("research", "one-shot guidance")}
	if err := testApp.app.runQuestion(context.Background(), "$research summarize the repository"); err != nil {
		t.Fatalf("one-shot skill request failed: %v", err)
	}
	if len(testApp.requests) != 1 {
		t.Fatalf("model request count = %d, want 1", len(testApp.requests))
	}
	user := testApp.requests[0].Messages[len(testApp.requests[0].Messages)-1].Content
	if !strings.Contains(user, "one-shot guidance") || !strings.Contains(user, "User request:\nsummarize the repository") {
		t.Fatalf("one-shot request was not prepared: %q", user)
	}
}

func TestOneShotSkillReferenceHasEquivalentChatAndResponsesPrompt(t *testing.T) {
	var prompts [2]string
	for i, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat", true: "responses"}[responses], func(t *testing.T) {
			var payload map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode model request: %v", err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if responses {
					_, _ = w.Write([]byte(protocolTextResponse(true, "ok", "")))
				} else {
					_, _ = w.Write([]byte(chatTurnResponse("ok", "", nil)))
				}
			}))
			defer server.Close()

			endpoint := server.URL
			if responses {
				endpoint += "/responses"
			}
			a := &app{
				cfg:    config{workspaceRoot: t.TempDir(), model: "test-model", finalOnly: true, maxIterations: 1},
				client: &client{endpoint: endpoint, model: "test-model", http: server.Client()},
				sink:   &spySink{},
				skills: map[string]skills.Skill{"research": testSkill("research", "adapter guidance")},
			}
			if err := a.runQuestion(context.Background(), "$research compare adapters"); err != nil {
				t.Fatalf("one-shot request failed: %v", err)
			}
			prompts[i] = extractPreparedPrompt(t, payload, responses)
		})
	}
	if prompts[0] != prompts[1] {
		t.Fatalf("chat and Responses prompts differ:\nchat=%q\nresponses=%q", prompts[0], prompts[1])
	}
}

func TestSkillReferenceDoesNotEnableExecuteSkill(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.skills = map[string]skills.Skill{"research": testSkill("research", "guidance")}
	session := &interactiveSession{messages: []types.Message{{Role: "system", Content: "test"}}, runtime: testApp.app.rootRuntime()}
	if testApp.app.runInteractiveTurn(context.Background(), session, "$research perform the task") {
		t.Fatal("skill selection stopped the interactive session")
	}
	if session.runtime.allowedTools[toolExecuteSkill] {
		t.Fatal("skill selection enabled execute_skill")
	}
	if len(testApp.requests) != 1 || len(testApp.requests[0].Tools) != 0 {
		t.Fatalf("skill selection changed disabled tool exposure: %#v", testApp.requests)
	}
}

func TestServerSkillReferenceRemainsOrdinaryUserText(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatTurnResponse("ok", "", nil)))
	}))
	defer backend.Close()

	a := &app{cfg: config{model: "test-model", reasoning: "medium"}}
	req := httptest.NewRequest(http.MethodPost, "/?endpoint="+url.QueryEscape(backend.URL), strings.NewReader(`{"messages":[{"role":"user","content":"$research keep this text"}]}`))
	req.Header.Set("Authorization", "Bearer token")
	prepared, err := a.prepareServerRequest(req, map[string]bool{}, "")
	if err != nil {
		t.Fatalf("prepare server request: %v", err)
	}
	if prepared.Question != "$research keep this text" {
		t.Fatalf("server request changed skill reference: %q", prepared.Question)
	}
}

func extractPreparedPrompt(t *testing.T, payload map[string]any, responses bool) string {
	t.Helper()
	if !responses {
		messages, ok := payload["messages"].([]any)
		if !ok || len(messages) == 0 {
			t.Fatalf("chat request has no messages: %#v", payload)
		}
		message := messages[len(messages)-1].(map[string]any)
		return message["content"].(string)
	}
	items, ok := payload["input"].([]any)
	if !ok {
		t.Fatalf("Responses request has no input: %#v", payload)
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["role"] == "user" {
			return item["content"].(string)
		}
	}
	t.Fatalf("Responses request has no user input: %#v", payload)
	return ""
}

func runeStrings(values [][]rune) []string {
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = string(value)
	}
	return result
}
