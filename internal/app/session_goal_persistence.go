package app

import sessionpkg "capelin-go/internal/sessions"

func toSessionGoal(state *goalState) *sessionpkg.Goal {
	if state == nil {
		return nil
	}
	result := &sessionpkg.Goal{Objective: state.Objective, Generation: state.Generation}
	if state.Completion != nil {
		result.Completion = &sessionpkg.GoalCompletion{
			Summary:              state.Completion.Summary,
			Evidence:             append([]string(nil), state.Completion.Evidence...),
			Generation:           state.Completion.Generation,
			ChecklistFingerprint: state.Completion.ChecklistFingerprint,
		}
	}
	return result
}

func fromSessionGoal(state *sessionpkg.Goal) *goalState {
	if state == nil {
		return nil
	}
	result := &goalState{Objective: state.Objective, Generation: state.Generation}
	if state.Completion != nil {
		result.Completion = &goalCompletion{
			Summary:              state.Completion.Summary,
			Evidence:             append([]string(nil), state.Completion.Evidence...),
			Generation:           state.Completion.Generation,
			ChecklistFingerprint: state.Completion.ChecklistFingerprint,
		}
	}
	return result
}
