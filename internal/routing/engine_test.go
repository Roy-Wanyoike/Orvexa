package routing

import (
	"testing"
)

var testAgents = []AgentView{
	{ID: "agent-a", DisplayName: "Amara", Language: "sw", Skills: []string{"billing", "swahili"}, QueueID: "q1"},
	{ID: "agent-b", DisplayName: "Brian", Language: "en", Skills: []string{"billing"}, QueueID: "q1"},
	{ID: "agent-c", DisplayName: "Cynthia", Language: "en", Skills: []string{"escalations"}, QueueID: "q2"},
}

func allAvailable() map[string]bool {
	return map[string]bool{"agent-a": true, "agent-b": true, "agent-c": true}
}

func TestEnginePrefersFullCoverageAndLanguage(t *testing.T) {
	e := Engine{}
	req := Request{RequiredSkills: []string{"billing"}, Language: "sw", Priority: 5}
	d := e.Decide(req, testAgents, allAvailable())
	if d.Outcome != OutcomeAssigned {
		t.Fatalf("want assignment, got %s (%v)", d.Outcome, d.Candidates)
	}
	if d.AssignedAgentID != "agent-a" {
		t.Fatalf("Swahili billing agent must win, got %s", d.AssignedAgentID)
	}
}

func TestEngineUnavailableAgentsNeverAssigned(t *testing.T) {
	e := Engine{}
	req := Request{RequiredSkills: []string{"billing"}, Language: "sw", Priority: 5}
	avail := map[string]bool{"agent-b": true} // only Brian available
	d := e.Decide(req, testAgents, avail)
	if d.AssignedAgentID != "agent-b" {
		t.Fatalf("only available skilled agent must win, got %s", d.AssignedAgentID)
	}
	// the unavailable top-match must still appear, ranked, available=false
	if len(d.Candidates) == 0 || d.Candidates[0].AgentID != "agent-a" || d.Candidates[0].Available {
		t.Fatalf("unavailable candidate must rank (unavailable), got %+v", d.Candidates)
	}
}

func TestEngineUrgentRequiresFullCoverage(t *testing.T) {
	e := Engine{}
	req := Request{RequiredSkills: []string{"billing", "swahili"}, Language: "sw", Priority: 8}
	// Brian: partial coverage, available → must NOT be assigned at priority 8
	d := e.Decide(req, testAgents, map[string]bool{"agent-b": true})
	if d.Outcome == OutcomeAssigned {
		t.Fatalf("urgent request must not assign partial coverage: %+v", d.AssignedAgentID)
	}
	if d.Outcome != OutcomeQueued && d.Outcome != OutcomeNone {
		t.Fatalf("unexpected outcome %s", d.Outcome)
	}
}

func TestEngineQueuePinning(t *testing.T) {
	e := Engine{}
	req := Request{RequiredSkills: []string{"escalations"}, QueueID: "q2", Priority: 5}
	d := e.Decide(req, testAgents, allAvailable())
	if d.AssignedAgentID != "agent-c" {
		t.Fatalf("queue-pinned request must stay in queue, got %s", d.AssignedAgentID)
	}
}

func TestEngineNoCandidatesMeansNoneAvailable(t *testing.T) {
	e := Engine{}
	d := e.Decide(Request{Priority: 5}, nil, nil)
	if d.Outcome != OutcomeNone {
		t.Fatalf("no candidates must yield no_agent_available, got %s", d.Outcome)
	}
}

func TestEngineLowPriorityFallsBackGracefully(t *testing.T) {
	e := Engine{}
	// no skills required, someone available → assign best-effort
	d := e.Decide(Request{Priority: 3}, testAgents, map[string]bool{"agent-c": true})
	if d.Outcome != OutcomeAssigned || d.AssignedAgentID != "agent-c" {
		t.Fatalf("best-effort fallback must assign, got %s %s", d.Outcome, d.AssignedAgentID)
	}
	// nobody available → queued
	d = e.Decide(Request{Priority: 3}, testAgents, nil)
	if d.Outcome != OutcomeQueued {
		t.Fatalf("no availability must queue, got %s", d.Outcome)
	}
}

func TestDecisionIsDeterministic(t *testing.T) {
	e := Engine{}
	req := Request{RequiredSkills: []string{"billing"}, Language: "en", Priority: 5}
	a := e.Decide(req, testAgents, allAvailable())
	b := e.Decide(req, testAgents, allAvailable())
	if a.AssignedAgentID != b.AssignedAgentID || a.Outcome != b.Outcome {
		t.Fatal("identical inputs must produce identical outcomes")
	}
	for i := range a.Candidates {
		if a.Candidates[i].Score != b.Candidates[i].Score {
			t.Fatal("scoring must be deterministic")
		}
	}
}
