package domain

import "testing"

func TestStatePredicates(t *testing.T) {
	for _, s := range []State{StateAdded, StateRemoved, StateChanged, StateUnchanged, StateUnknown, StateFailed, StateDenied, StateSkipped, ""} {
		want := s == "added" || s == "changed" || s == "removed"
		if got := s.IsChange(); got != want {
			t.Errorf("State(%q).IsChange() = %t, want %t", s, got, want)
		}
	}
	for _, e := range []Execution{ExecutionComplete, ExecutionFailed, ExecutionDenied, ExecutionSkipped, ""} {
		want := e == "failed" || e == "denied"
		if got := e.FailedOrDenied(); got != want {
			t.Errorf("Execution(%q).FailedOrDenied() = %t, want %t", e, got, want)
		}
	}
}
