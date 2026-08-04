package agent

import (
	"context"
	"testing"

	"capelin-go/internal/contracts"
)

type continuationTestProvider struct {
	testProvider
	restored *contracts.ContinuationState
}

func (p *continuationTestProvider) InitializeWithContinuation(messages []contracts.Message, question string, state *contracts.ContinuationState) contracts.TurnState {
	p.restored = state
	return p.Initialize(messages, question)
}

func (*continuationTestProvider) ContinuationState(contracts.TurnState) *contracts.ContinuationState {
	return &contracts.ContinuationState{Provider: "test", Version: 1}
}

func TestEngineCarriesOpaqueProviderStateWithoutInterpretingIt(t *testing.T) {
	provider := &continuationTestProvider{testProvider: testProvider{responses: []contracts.Completion{testCompletion{content: "done"}}}}
	continuation := &contracts.ContinuationState{Provider: "opaque", Version: 9, Data: []byte(`{"wire":"state"}`)}
	result, err := (&Engine{Provider: provider}).Run(context.Background(), RunOptions{
		Question:          "continue",
		ContinuationState: continuation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.restored != continuation {
		t.Fatalf("engine interpreted or copied provider state: got %#v, want same opaque value", provider.restored)
	}
	if result.ContinuationState == nil || result.ContinuationState.Provider != "test" || result.ContinuationState.Version != 1 {
		t.Fatalf("result continuation=%#v", result.ContinuationState)
	}
}
