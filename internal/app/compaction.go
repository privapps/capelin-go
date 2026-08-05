package app

import (
	"capelin-go/internal/agent"
	"capelin-go/internal/contracts"
	"context"
	"errors"
	"fmt"
	"strings"
)

const compactedHistoryMarker = "[CAPELIN COMPACTED HISTORY]\n"

func (a *app) compactAndPersistInteractiveSession(ctx context.Context, session *interactiveSession) (int, error) {
	worker := cloneInteractiveSession(a, session)
	if worker == nil {
		return 0, errors.New("interactive session is nil")
	}
	if err := a.compactInteractiveSession(ctx, worker); err != nil {
		return 0, err
	}
	if err := a.saveInteractiveSessionCandidate(worker); err != nil {
		return 0, err
	}
	commitInteractiveTurn(session, worker)
	session.successMessage = ""
	a.attachInteractiveRuntime(session)
	a.attachInteractiveSaver(session)
	return len(session.messages), nil
}

func (a *app) compactInteractiveSession(ctx context.Context, session *interactiveSession) error {
	if session == nil {
		return errors.New("interactive session is nil")
	}
	if len(session.messages) <= 1 {
		return errNothingToCompact
	}
	messages := cloneMessages(session.messages)
	model, reasoning := "", ""
	if a != nil && a.client != nil {
		model, reasoning = a.client.model, a.client.reasoning
	}
	if session.runtime != nil && session.runtime.model != "" {
		model, reasoning = session.runtime.model, session.runtime.reasoning
	}
	summary, err := (&agent.Engine{Provider: a.client.agentProvider()}).Compact(ctx, agent.CompactOptions{
		Messages: messages, Model: model, Reasoning: reasoning, Sink: a.sink, AgentID: rootAgentID,
	})
	if err != nil {
		return err
	}
	compacted := make([]contracts.Message, 0, 2)
	if system := firstSystemMessage(messages); system != nil {
		compacted = append(compacted, *system)
	}
	compacted = append(compacted, contracts.Message{Role: "assistant", Content: compactedHistoryMarker + summary})
	session.messages = compacted
	session.providerState = nil
	session.loadedSkills = make(map[string]bool)
	return nil
}

var errNothingToCompact = errors.New("nothing to compact")

func firstSystemMessage(messages []contracts.Message) *contracts.Message {
	for _, message := range messages {
		if message.Role == "system" {
			clone := message
			clone.ToolCalls = append([]contracts.ToolCall(nil), message.ToolCalls...)
			return &clone
		}
	}
	return nil
}

func compactUsageError(arg string) error {
	if strings.TrimSpace(arg) == "" {
		return nil
	}
	return fmt.Errorf("/compact accepts no arguments")
}
