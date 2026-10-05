package report

import (
	"math"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
)

// withReadings gives each run of the report a usage reading that rises 0.5 points of the window over the run, one run
// after another, in one window.
func withReadings(rep Report) Report {
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	resets := start.Add(4 * time.Hour)
	for i := range rep.Runs {
		at := start.Add(time.Duration(i) * time.Minute)
		rep.Runs[i].Started = at.Format(time.RFC3339)
		rep.Runs[i].Finished = at.Add(50 * time.Second).Format(time.RFC3339)
		rep.Runs[i].Metrics.UsageFirst = &agent.UsageReading{FiveHour: 0.18 + 0.005*float64(i), FiveHourResets: resets}
		rep.Runs[i].Metrics.UsageLast = &agent.UsageReading{FiveHour: 0.18 + 0.005*float64(i+1), FiveHourResets: resets}
	}
	return rep
}

// The share is the preview's estimate over the runs' readings: 20 runs that raised the window 18% to 28% used 10%.
func TestPlanShareFromReadings(t *testing.T) {
	rep, err := Build(oneRun("phase1-v2", "context-ab"))
	if err != nil {
		t.Fatal(err)
	}
	rep.Lock.SignIn = claude.SignInLogin
	rep = withReadings(rep)
	share, ok := PlanShare(rep)
	if !ok || math.Abs(share-0.005*float64(len(rep.Runs))) > 1e-9 {
		t.Errorf("share %v ok %v over %d runs, want %v", share, ok, len(rep.Runs), 0.005*float64(len(rep.Runs)))
	}
	// Too few readings in a window to trust: the default rate is a guess, so there is no share.
	few := withReadings(rep)
	for i := 2; i < len(few.Runs); i++ {
		few.Runs[i].Metrics.UsageFirst, few.Runs[i].Metrics.UsageLast = nil, nil
	}
	if share, ok := PlanShare(few); ok {
		t.Errorf("two readings gave a share of %v", share)
	}
}

// An API key reports no usage, even with readings in the records; neither does a lock that never recorded a sign-in.
func TestPlanShareAPIKey(t *testing.T) {
	rep, err := Build(oneRun("phase1-v2", "context-ab"))
	if err != nil {
		t.Fatal(err)
	}
	rep = withReadings(rep)
	for _, signIn := range []string{claude.SignInAPIKey, ""} {
		rep.Lock.SignIn = signIn
		if share, ok := PlanShare(rep); ok {
			t.Errorf("sign-in %q: share %v, want none", signIn, share)
		}
	}
}

// A model A/B's arms run side by side, so the window's rise is theirs together: 16 runs in pairs of two models that
// raised the window from 18% to 22% used 4%, not 4% for each model (which measuring each model alone would give).
func TestPlanShareOverlappingModels(t *testing.T) {
	rep, err := Build(oneRun("phase1-v2", "context-ab"))
	if err != nil {
		t.Fatal(err)
	}
	rep.Lock.SignIn = claude.SignInLogin
	rep.Lock.Design.Arms[0].Model, rep.Lock.Design.Arms[1].Model = "model-a", "model-b"
	rep.Runs = rep.Runs[:16]
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	resets := start.Add(4 * time.Hour)
	for i := range rep.Runs {
		pair := i / 2 // both runs of a pair run at the same time
		at := start.Add(time.Duration(pair) * time.Minute)
		rep.Runs[i].Started, rep.Runs[i].Finished = at.Format(time.RFC3339), at.Add(50*time.Second).Format(time.RFC3339)
		rep.Runs[i].Metrics.UsageFirst = &agent.UsageReading{FiveHour: 0.18 + 0.005*float64(pair), FiveHourResets: resets}
		rep.Runs[i].Metrics.UsageLast = &agent.UsageReading{FiveHour: 0.18 + 0.005*float64(pair+1), FiveHourResets: resets}
	}
	share, ok := PlanShare(rep)
	if !ok || math.Abs(share-0.04) > 1e-9 {
		t.Errorf("share %v ok %v, want 0.04", share, ok)
	}
}
