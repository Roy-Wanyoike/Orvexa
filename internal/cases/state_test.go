package cases

import "testing"

func TestCaseLifecycleHappyPaths(t *testing.T) {
	paths := [][]Status{
		{StatusOpen, StatusInProgress, StatusResolved, StatusClosed},
		{StatusOpen, StatusInProgress, StatusPendingCustomer, StatusInProgress, StatusResolved, StatusClosed},
		{StatusOpen, StatusResolved, StatusClosed},
		{StatusOpen, StatusCanceled},
	}
	for _, path := range paths {
		for i := 0; i < len(path)-1; i++ {
			if !CanTransition(path[i], path[i+1]) {
				t.Fatalf("legal path step %s→%s rejected", path[i], path[i+1])
			}
		}
	}
}

func TestCaseReopenAllowedFromResolved(t *testing.T) {
	if !CanTransition(StatusResolved, StatusInProgress) {
		t.Fatal("resolved→in_progress (reopen) must be legal")
	}
}

func TestCaseIllegalTransitions(t *testing.T) {
	illegal := []struct{ from, to Status }{
		{StatusOpen, StatusClosed},       // must resolve first
		{StatusOpen, StatusPendingCustomer},
		{StatusClosed, StatusInProgress}, // reopen of closed forbidden
		{StatusCanceled, StatusOpen},
		{StatusClosed, StatusClosed},
	}
	for _, c := range illegal {
		if CanTransition(c.from, c.to) {
			t.Errorf("illegal transition %s→%s accepted", c.from, c.to)
		}
	}
}
