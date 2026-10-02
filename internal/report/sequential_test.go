package report

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/term"
)

// seqInput is a seq-v1 context A/B of 16 tasks with runs for the slots before end: arm B costs ratio times arm A.
func seqInput(t *testing.T, ratio float64, end int, futility bool, status string) Input {
	t.Helper()
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	d := experiment.Design{Version: experiment.DesignVersionSeq, Method: experiment.MethodSeq, Template: experiment.TemplateContextAB,
		Arms:    []experiment.Arm{{Name: "A", Context: "base"}, {Name: "B", Context: "lean", Snapshot: "b808beb3"}},
		Repeats: 1, Model: "claude-sonnet-5", Goal: experiment.GoalCheaper, CostMargin: 0.10, SuccessMargin: 0.15, RunBudgetUSD: 3, BudgetUSD: 60,
		Timeout: 20 * time.Minute, VerifyTimeout: 10 * time.Minute, Concurrency: 2, Seed: 42, NoFutility: !futility}
	for i := range 16 {
		d.Tasks = append(d.Tasks, fmt.Sprintf("task-%02d", i))
	}
	seq, err := experiment.NewSequential(16, futility)
	if err != nil {
		t.Fatal(err)
	}
	l := experiment.Lock{Method: experiment.MethodSeq, LockedAt: at, ClaudeCode: "2.1.281", SignIn: claude.SignInLogin, PriceTable: "2026-09-29",
		Design: d, Schedule: experiment.Schedule(d), MaxAttempts: 3, Sequential: &seq}
	for _, a := range d.Arms {
		l.Arms = append(l.Arms, experiment.LockedArm{Arm: a, Model: "claude-sonnet-5"})
	}
	for _, name := range d.Tasks {
		l.Tasks = append(l.Tasks, experiment.LockedTask{Name: name})
	}
	in := Input{Name: "lean-seq", Lock: l, Status: status}
	yes := true
	for _, s := range l.Schedule[:end] {
		var ti int
		fmt.Sscanf(s.Task, "task-%d", &ti)
		cost := 0.30 * (1 + 0.2*float64(ti%4)) * (1 + 0.03*float64((s.Position*7)%11))
		if s.Arm == "B" {
			cost *= ratio
		}
		rec := run.Record{ID: fmt.Sprintf("r%02d", s.Position), Task: s.Task, Arm: s.Arm, Model: d.Model, Outcome: claude.OutcomeOK, Passed: &yes,
			Started: at.Add(time.Duration(s.Position) * time.Minute), Finished: at.Add(time.Duration(s.Position)*time.Minute + 50*time.Second),
			Metrics: claude.Metrics{CLIVersion: "2.1.281", Model: "claude-sonnet-5", CostUSD: cost, DurationMS: 40000, OutputTokens: 3000, SawInit: true, SawResult: true}}
		in.Runs = append(in.Runs, Run{ID: rec.ID, Slot: s.Position, Attempt: 1, Record: rec})
	}
	return in
}

func renderings(t *testing.T, rep Report) (md, plain string) {
	t.Helper()
	var m, p bytes.Buffer
	if err := rep.Markdown(&m); err != nil {
		t.Fatal(err)
	}
	if err := rep.Terminal(&p, term.Style{}); err != nil {
		t.Fatal(err)
	}
	return m.String(), p.String()
}

// An early stop: the report says where it stopped, shows the look's interval at its level, lists the looks, and warns
// that an early stop overstates the effect.
func TestSeqReportOfAnEarlyStop(t *testing.T) {
	rep, err := Build(seqInput(t, 0.5, 16, true, experiment.StatusDone))
	if err != nil {
		t.Fatal(err)
	}
	md, plain := renderings(t, rep)
	for _, want := range []string{"Method seq-v1: stopped at look 1 of 3 (8 tasks): cost improved.", "(99.84%: ", "): improved.",
		"## Looks", "| 1 of 3 | 8 of 8 |", "| 99.84% | improved | - | stop |", "| Bootstrap | t |", "cost's at 99.84%, its look's level",
		"An early stop overstates the effect's size on average", "The results are look 1's", "cost's at its look's levels"} {
		if !strings.Contains(md, want) {
			t.Errorf("the Markdown lacks %q:\n%s", want, md)
		}
	}
	for _, want := range []string{"Method seq-v1: stopped at look 1 of 3 (8 tasks): cost improved.", "Looks", "1 of 3", "stop", "early stop overstates"} {
		if !strings.Contains(plain, want) {
			t.Errorf("the terminal report lacks %q:\n%s", want, plain)
		}
	}
	for _, phase1 := range []string{"95% bootstrap", "8–12 tasks × 1 run", "at 95%, or at 90% for \"no loss\" and \"equivalent\""} {
		if strings.Contains(md, phase1) {
			t.Errorf("a seq-v1 report says phase1-v2's %q", phase1)
		}
	}
	if s := rep.Analysis.Sequential; s == nil || s.Reported != 1 || s.Ended != experiment.LookStop || rep.Analysis.Results[1].Tasks != 8 {
		t.Errorf("the analysis %+v", rep.Analysis.Sequential)
	}
}

// Between looks (a budget stop in stage 2): the results are look 1's, the later runs are named as left out, and there
// is no early-stop warning.
func TestSeqReportBetweenLooks(t *testing.T) {
	rep, err := Build(seqInput(t, 1.0, 20, false, experiment.StatusBudget))
	if err != nil {
		t.Fatal(err)
	}
	md, _ := renderings(t, rep)
	for _, want := range []string{"Method seq-v1: look 1 of 3 made (continue); look 2 comes once the first 12 tasks are settled.",
		"20 of 32 runs settled, and the results are look 1's.", "The results are look 1's",
		"4 run(s) of stages after look 1 are not in its results (their spend is in the total): the next look counts them once its stage is settled.",
		"futility stops were off", "| 1 of 3 | 8 of 8 |", "| continue |"} {
		if !strings.Contains(md, want) {
			t.Errorf("the Markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "early stop overstates") {
		t.Errorf("a stop between looks is not an early stop:\n%s", md)
	}
	if cost := rep.Analysis.Results[1]; cost.Tasks != 8 || cost.Verdict != "inconclusive" {
		t.Errorf("cost between looks: %+v", cost)
	}
	// Before any look the report says so, and cost has no verdict.
	none, err := Build(seqInput(t, 0.5, 10, true, experiment.StatusStopped))
	if err != nil {
		t.Fatal(err)
	}
	md, _ = renderings(t, none)
	if !strings.Contains(md, "No look was analysed yet, so cost has no verdict") || !strings.Contains(md, "): exploratory: no look yet") ||
		strings.Contains(md, "## Looks") || strings.Contains(md, "too small") || none.Analysis.Results[1].Verdict != "exploratory" {
		t.Errorf("before any look:\n%s", md)
	}
}

// When the look after the reported one was made but gave no verdict (every run of its stage failed), the note says so
// rather than promising that the next look counts the runs.
func TestSeqReportAfterALookWithoutVerdict(t *testing.T) {
	in := seqInput(t, 1.0, 26, false, experiment.StatusStopped)
	var runs []Run
	for _, r := range in.Runs {
		if r.Slot >= 16 && r.Slot < 24 { // stage 2: three infrastructure failures a slot
			for a := 1; a <= 3; a++ {
				failed := r
				failed.Attempt, failed.Record.Outcome, failed.Record.Passed = a, claude.OutcomeInfra, nil
				runs = append(runs, failed)
			}
			continue
		}
		runs = append(runs, r)
	}
	in.Runs = runs
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := renderings(t, rep)
	for _, want := range []string{"| 2 of 3 | 8 of 12 | - | - | - | no verdict: no task was counted since the last analysed look | - | continue |",
		"look 2 was made on them but gave no verdict (no task was counted since the last analysed look), so the results stay look 1's"} {
		if !strings.Contains(md, want) {
			t.Errorf("the Markdown lacks %q:\n%s", want, md)
		}
	}
}

// The smoke check's case: a budget stop before the first look. The note on the unfinished experiment says no look was
// made, rather than that the results are its last look's.
func TestSeqReportOfABudgetStopBeforeAnyLook(t *testing.T) {
	rep, err := Build(seqInput(t, 1.0, 6, true, experiment.StatusBudget))
	if err != nil {
		t.Fatal(err)
	}
	md, plain := renderings(t, rep)
	for _, out := range []string{md, plain} {
		if !strings.Contains(out, "6 of 32 runs settled, and no look has been analysed yet, so the results cover every run so far.") ||
			strings.Contains(out, "last look's") || strings.Contains(out, "are look ") {
			t.Errorf("a budget stop before any look:\n%s", out)
		}
	}
}

// A run Claude Code stopped at its cap counts as it ended (graded, at the cost it reached), and the report marks it:
// its task's counts and mean cost in the per-task table, a note with the count per arm, and the arms' JSON.
func TestReportMarksCappedRuns(t *testing.T) {
	in := seqInput(t, 1.0, 6, true, experiment.StatusBudget)
	var capped string
	for i, r := range in.Runs {
		if r.Record.Arm == "B" {
			in.Runs[i].Record.Outcome, in.Runs[i].Record.Metrics.CostUSD = claude.OutcomeCapped, 3.07
			in.Runs[i].Record.Metrics.Result = "error_max_budget_usd"
			capped = r.Record.Task
			break
		}
	}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Arms[0].Capped != 0 || rep.Arms[1].Capped != 1 || rep.Arms[1].Counted != 3 {
		t.Errorf("arms: A %d capped, B %d capped of %d counted", rep.Arms[0].Capped, rep.Arms[1].Capped, rep.Arms[1].Counted)
	}
	for _, row := range rep.Tasks {
		if want := map[bool]int{true: 1}[row.Task == capped]; row.Arms["B"].Capped != want || row.Arms["A"].Capped != 0 {
			t.Errorf("task %s: %+v", row.Task, row.Arms)
		}
		if row.Task == capped && (row.Arms["B"].Successes != 1 || row.Arms["B"].Counted != 1) {
			t.Errorf("a capped run that passed counts as a success, as before: %+v", row.Arms["B"])
		}
	}
	md, plain := renderings(t, rep)
	note := "1 counted run(s) were capped (A 0, B 1): Claude Code stopped them at their cost cap or turn limit, so each one's cost is a lower bound"
	for _, out := range []string{md, plain} {
		for _, want := range []string{"1/1, 1 capped", "→ ≥$3.070", "≥ marks a mean that includes one", note} {
			if !strings.Contains(out, want) {
				t.Errorf("the report lacks %q:\n%s", want, out)
			}
		}
	}
	if !strings.Contains(md, "| "+capped+" | ● 1/1 | ● 1/1, 1 capped | $0.436 → ≥$3.070 |") || strings.Contains(md, "The arms' caps differ") {
		t.Error("a context experiment's arms share one cap")
	}
	// Without capped runs, the legend and the notes are as they were.
	plainRep, err := Build(seqInput(t, 1.0, 6, true, experiment.StatusBudget))
	if err != nil {
		t.Fatal(err)
	}
	if md, _ := renderings(t, plainRep); strings.Contains(md, "capped") || strings.Contains(md, "≥") {
		t.Errorf("a report without capped runs mentions them:\n%s", md)
	}
}

// A model-ab experiment whose arms have their own caps says the lower one cuts its arm shorter.
func TestCappedNoteWithDifferentArmCaps(t *testing.T) {
	rep := Report{Arms: []Arm{{Name: "A", Capped: 2}, {Name: "B"}}, Lock: experiment.Lock{Design: experiment.Design{Template: experiment.TemplateModelAB,
		RunBudgetUSD: 3, Arms: []experiment.Arm{{Name: "A", Model: "m1", RunBudgetUSD: 1}, {Name: "B", Model: "m2"}}}}}
	if note := cappedNote(rep); !strings.HasPrefix(note, "2 counted run(s) were capped (A 2, B 0)") || !strings.HasSuffix(note, "the arm with the lower cap is cut shorter.") {
		t.Errorf("note %q", note)
	}
	rep.Lock.Design.Arms[0].RunBudgetUSD = 3
	if note := cappedNote(rep); strings.Contains(note, "caps differ") {
		t.Errorf("equal caps: %q", note)
	}
	rep.Arms[0].Capped = 0
	if cappedNote(rep) != "" {
		t.Error("no capped runs, no note")
	}
}
