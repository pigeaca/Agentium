// Package reporttest builds experiments' stored data for the tests of reports and of their terminal views: a lock and
// real-looking run records, as report.Load reads them. It returns plain data (Experiment), so the report package's own
// tests can use it without an import cycle; report.Input converts from it field by field.
package reporttest

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/judge"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/task"
)

// Run is one stored run of an experiment, as report.Run.
type Run struct {
	ID      string
	Slot    int
	Attempt int
	Record  run.Record
}

// Experiment is what a report is made from, as report.Input: its lock, status and runs, and the local folders the
// report names as placeholders.
type Experiment struct {
	Name       string
	Lock       experiment.Lock
	Status     string
	StatusNote string
	Runs       []Run
	DataDir    string
	Home       string
}

// LeanAB is a context A/B of 10 tasks × 3 runs per arm: arm B's context is smaller and cheaper, success equal but
// mixed; one run of each kind that is not counted; one pass with changed runner configuration; one recovered run.
func LeanAB() Experiment {
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	d := experiment.Design{Version: experiment.DesignVersion, Template: experiment.TemplateContextAB,
		Arms:    []experiment.Arm{{Name: "A", Context: "base"}, {Name: "B", Context: "lean", Snapshot: "b808beb3ddbaea19f7643ae0c20ec8167641da46"}},
		Repeats: 3, Model: "claude-sonnet-5", Goal: experiment.GoalCheaper, CostMargin: 0.10, SuccessMargin: 0.15, RunBudgetUSD: 3, BudgetUSD: 60,
		Timeout: 20 * time.Minute, VerifyTimeout: 10 * time.Minute, Concurrency: 2, Seed: 42}
	for i := range 10 {
		d.Tasks = append(d.Tasks, fmt.Sprintf("task-%d", i))
	}
	l := experiment.Lock{Method: experiment.MethodV2, Agentium: "test", LockedAt: at, ClaudeCode: "2.1.281", ClaudePath: "/usr/local/bin/claude",
		SignIn: claude.SignInLogin, Host: "darwin/arm64", PriceTable: "2026-09-29", Design: d, Schedule: experiment.Schedule(d), MaxAttempts: 3}
	for _, a := range d.Arms {
		l.Arms = append(l.Arms, experiment.LockedArm{Arm: a, Calibration: "cal-" + a.Name, Model: "claude-sonnet-5", Tools: []string{"Bash", "Edit", "Read"},
			Skills: []string{"personal-looking-skill"}, SlashCommands: []string{"compact", "review"}})
	}
	for _, name := range d.Tasks {
		l.Tasks = append(l.Tasks, experiment.NewLockedTask(name, "Fix "+name+".", task.Spec{Base: "base-commit", Verify: []string{"make test"}}))
	}
	var runs []Run
	yes, no := true, false
	for _, s := range l.Schedule {
		var ti int
		fmt.Sscanf(s.Task, "task-%d", &ti)
		cost := 0.30 * (1 + 0.25*float64(ti%3)) * (1 + 0.04*float64(s.Repeat))
		first := int64(30000)
		if s.Arm == "B" {
			cost *= 0.8
			first = 27000
		}
		passed := &yes
		if (ti+s.Repeat)%4 == 0 {
			passed = &no
		}
		isolated := cost + 0.02 // the first request's reads repriced as writes: a little above the actual cost
		rec := run.Record{ID: fmt.Sprintf("r%02d", s.Position), Task: s.Task, Arm: s.Arm, Model: d.Model, SignIn: claude.SignInLogin,
			IsolatedCostUSD: &isolated, Outcome: claude.OutcomeOK, Passed: passed, ContextHead: "ctx", RecordsDir: "/home/someone/.agentium/records/x",
			Started: at.Add(time.Duration(s.Position) * time.Minute), Finished: at.Add(time.Duration(s.Position)*time.Minute + 50*time.Second),
			Metrics: claude.Metrics{CLIVersion: "2.1.281", Model: "claude-sonnet-5", CostUSD: cost, DurationMS: int64(40000 + 1000*ti), InputTokens: 50,
				OutputTokens: int64(3000 + 100*ti), CacheReadTokens: 400000, CacheWriteTokens: 30000, FirstRequest: first, SawInit: true, SawResult: true},
			Behavior: run.Behavior{FilesChanged: 2, LinesAdded: 10, LinesRemoved: 3, TestsChanged: s.Arm == "A", RanTests: true, RanChecks: s.Arm == "A",
				BashCommands: 6}, Verify: []task.Command{{Command: "make test", ExitCode: 0, Seconds: 1.5}}}
		// What the runs used of their context: arm A reads the testing doc and invokes its skill more than lean B; one
		// counted run predates context use.
		use := &run.ContextUse{Start: []string{"AGENTS.md", "CLAUDE.md"}}
		if s.Arm == "B" {
			use.Start = []string{"CLAUDE.md"}
		}
		if ti%2 == 0 && (s.Arm == "A" || ti%4 == 0) {
			use.Files = []string{"docs/testing.md"}
		}
		if s.Arm == "A" && ti%5 == 0 {
			use.Skills = []string{"review-change"}
		}
		if ti == 3 && s.Repeat == 1 {
			use.Subagents = []string{"Explore"}
		}
		if ti == 5 && s.Arm == "B" { // a subagent type that is neither the project's nor Claude Code's: counted, not named
			use.OtherSubagents = 1
		}
		if s.Position != 12 {
			rec.ContextUse = use
		}
		switch s.Position {
		case 3:
			rec.Outcome, rec.Passed = claude.OutcomeUnfair, nil
			rec.Drift = []string{"tools differ (added Monitor; missing none)", "1 file tool call(s) reached /home/someone/.agentium/projects"}
		case 5: // what the agent said, and a note naming local paths, stay out of the report
			rec.Metrics.ResultExcerpt = "Done in /home/someone/.agentium/workspaces/e1-s5-t1/repo with key sk-ant-api03-" + strings.Repeat("x", 40) // secret-scan: allow
			rec.Notes = []string{"the hidden tests could not be added: open /home/someone/.agentium/records/r05/verify/t: denied",
				"Agentium could not finish the run: sign-in with sk-ant-api03-" + strings.Repeat("y", 40) + " refused"} // secret-scan: allow
		case 7: // passed with changed runner configuration: a failure
			rec.Behavior.ConfigChanged, rec.Passed = []string{"pytest.ini"}, &yes
		case 9: // failed with it: a failure either way, not a lost pass
			rec.Behavior.ConfigChanged, rec.Passed = []string{"pytest.ini"}, &no
		}
		runs = append(runs, Run{ID: rec.ID, Slot: s.Position, Attempt: 1, Record: rec})
		if s.Position == 10 { // an earlier attempt that failed for infrastructure, and one recovered after a stop
			infra := rec
			infra.ID, infra.Outcome, infra.Passed = "r10-infra", claude.OutcomeInfra, nil
			infra.Metrics.CostUSD, infra.CostEstimated = 0.05, true
			cancelled := rec
			cancelled.ID, cancelled.Outcome, cancelled.Passed, cancelled.Recovered = "r10-cancelled", claude.OutcomeCancelled, nil, run.RecoveredStopped
			runs = append(runs[:len(runs)-1], Run{ID: infra.ID, Slot: 10, Attempt: 1, Record: infra}, Run{ID: cancelled.ID, Slot: 10, Attempt: 2, Record: cancelled},
				Run{ID: rec.ID, Slot: 10, Attempt: 2, Record: rec})
		}
	}
	return Experiment{Name: "lean-ab", Lock: l, Status: experiment.StatusDone, Runs: runs, DataDir: "/home/someone/.agentium", Home: "/home/someone"}
}

// OneRun is a 10-task × 1-run experiment locked under method: arm B about 20% cheaper with a spread across tasks (an
// A/B), or the same context twice (an A/A); log costs spread like Phase 0's (σ = 0.19); success mixed.
func OneRun(method, template string) Experiment {
	in := LeanAB()
	l := &in.Lock
	l.Method, l.Design.Template, l.Design.Repeats, l.Design.BudgetUSD = method, template, 1, 30
	name := "lean-ab-1run"
	if template == experiment.TemplateAA {
		l.Arms[1].Context, l.Arms[1].Snapshot = "base", ""
		l.Design.Arms[1] = l.Arms[1].Arm // after the change: the design's arm is the lock's
		name = "aa-1run"
	}
	l.Schedule = experiment.Schedule(l.Design)
	r := rand.New(rand.NewPCG(5, 6))
	effects := map[string]float64{}
	var runs []Run
	yes, no := true, false
	for _, s := range l.Schedule {
		if _, ok := effects[s.Task]; !ok {
			effects[s.Task] = math.Log(0.8) + 0.15*r.NormFloat64()
		}
		var ti int
		fmt.Sscanf(s.Task, "task-%d", &ti)
		cost := 0.30 * (1 + 0.25*float64(ti%3)) * math.Exp(0.19*r.NormFloat64())
		if s.Arm == "B" && template != experiment.TemplateAA {
			cost *= math.Exp(effects[s.Task])
		}
		passed := &yes
		if r.Float64() < 0.3 {
			passed = &no
		}
		rec := run.Record{ID: fmt.Sprintf("r%02d", s.Position), Task: s.Task, Arm: s.Arm, Model: l.Design.Model, SignIn: claude.SignInLogin,
			Outcome: claude.OutcomeOK, Passed: passed, ContextHead: "ctx", Started: l.LockedAt.Add(time.Duration(s.Position) * time.Minute),
			Finished: l.LockedAt.Add(time.Duration(s.Position)*time.Minute + 50*time.Second),
			Metrics: claude.Metrics{CLIVersion: "2.1.281", Model: "claude-sonnet-5", CostUSD: cost, DurationMS: int64(40000 + 1000*ti), InputTokens: 50,
				OutputTokens: int64(3000 + 100*ti), CacheReadTokens: 400000, CacheWriteTokens: 30000, FirstRequest: 30000, SawInit: true, SawResult: true},
			Behavior: run.Behavior{FilesChanged: 2, LinesAdded: 10, LinesRemoved: 3, RanTests: true, BashCommands: 6}}
		runs = append(runs, Run{ID: rec.ID, Slot: s.Position, Attempt: 1, Record: rec})
	}
	in.Name, in.Runs = name, runs
	return in
}

// Judged is fixture() with the judge on (3 repeats): every task has a reference in code but task-9, whose reference is
// only tests. Passing runs are judged fixed unless (task + repeat) % 3 is 0, when arm A's are "partly" and B's "no" (one
// without a majority); failing runs are judged "no". One run changed no code and one got no answer. A reason names a
// local path and a key, over two lines.
func Judged() Experiment {
	in := LeanAB()
	in.Name = "lean-ab-judged"
	l := &in.Lock
	l.Design.Judge = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 3}
	for i := range l.Tasks {
		reference := []string{"value.go", "value_test.go"}
		if i == 9 {
			reference = []string{"value_test.go"}
		}
		l.Tasks[i] = experiment.NewLockedTask(l.Tasks[i].Name, l.Tasks[i].Instruction, task.Spec{Base: "base-commit", Solution: "solution-commit",
			Reference: reference, Verify: []string{"make test"}})
	}
	for i := range in.Runs {
		r := &in.Runs[i]
		rec := &r.Record
		if rec.Passed == nil || rec.Task == "task-9" {
			continue
		}
		var ti int
		fmt.Sscanf(rec.Task, "task-%d", &ti)
		repeat := l.Schedule[r.Slot].Repeat
		v := judge.Verdict{Version: judge.Version, Requested: 3, Model: judge.DefaultModel, Effort: judge.DefaultEffort, CostUSD: 0.18 + 0.01*float64(ti)}
		answer := func(fixed, reason string, answers ...string) {
			v.Fixed, v.Reason, v.Answers = fixed, reason, answers
			for range answers {
				v.Reasons = append(v.Reasons, reason)
			}
		}
		switch {
		case r.Slot == 20:
			v.Empty, v.CostUSD, v.Answers, v.Reasons = true, 0, []string{}, []string{}
		case r.Slot == 21:
			v.Answers, v.Reasons, v.Errors = []string{}, []string{}, []string{"exit 1, not JSON: in /home/someone/.agentium/records/x/judge"}
		case !*rec.Passed:
			answer(judge.No, "The change does not touch the failing path.", judge.No, judge.No, judge.No)
		case (ti+repeat)%3 != 0:
			answer(judge.Yes, "Does what the reference does.", judge.Yes, judge.Yes, judge.Yes)
		case rec.Arm == "B" && ti == 2:
			answer(judge.Partly, "Handles the empty case but\n  not a missing file; see /home/someone/.agentium/records/x/agent.diff with sk-ant-api03-"+ // secret-scan: allow
				strings.Repeat("q", 40)+".", judge.Partly, judge.Partly, judge.Yes)
		case rec.Arm == "A":
			answer(judge.Partly, "Covers only the first of the two inputs the task names.", judge.Partly, judge.Partly, judge.Partly)
		case ti == 5:
			answer(judge.Partly, "", judge.Yes, judge.Partly, judge.No) // no majority
			v.Reasons = []string{"a", "b", "c"}
		default:
			answer(judge.No, "Works around the check instead of fixing the parser.", judge.No, judge.No, judge.Yes)
		}
		rec.Judge = &v
	}
	return in
}

// Seq is a seq-v1 context A/B of 16 tasks with runs for the slots before end, every one passing: arm B costs ratio
// times arm A. futility turns the futility stops on.
func Seq(ratio float64, end int, futility bool, status string) (Experiment, error) {
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
		return Experiment{}, err
	}
	l := experiment.Lock{Method: experiment.MethodSeq, LockedAt: at, ClaudeCode: "2.1.281", SignIn: claude.SignInLogin, PriceTable: "2026-09-29",
		Design: d, Schedule: experiment.Schedule(d), MaxAttempts: 3, Sequential: &seq}
	for _, a := range d.Arms {
		l.Arms = append(l.Arms, experiment.LockedArm{Arm: a, Model: "claude-sonnet-5"})
	}
	for _, name := range d.Tasks {
		l.Tasks = append(l.Tasks, experiment.LockedTask{Name: name})
	}
	e := Experiment{Name: "lean-seq", Lock: l, Status: status}
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
		e.Runs = append(e.Runs, Run{ID: rec.ID, Slot: s.Position, Attempt: 1, Record: rec})
	}
	return e, nil
}

// JudgedPairs is Judged with the pair judge on: of each pair whose two runs passed, lean's (B's) change is preferred in
// most, base's in some, and a few are ties, one of them a flip; one comparison is incomplete. A reason names a local
// path and a key, and one holds a table bar.
func JudgedPairs() Experiment {
	e := Judged()
	e.Name = "lean-ab-pairs"
	e.Lock.Design.JudgePairs = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 1}
	comparePairs(&e, func(n int, task string) judge.PairVerdict {
		v := judge.PairVerdict{Version: judge.PairVersion, Model: judge.DefaultModel, Effort: judge.DefaultEffort, CostUSD: 0.18,
			AB:     judge.PairOrder{Answered: true, Answer: "second", Reason: "Change 2 handles the empty input as the reference does.", CostUSD: 0.09},
			BA:     judge.PairOrder{Answered: true, Answer: "first", Reason: "Change 1 handles the empty input as the reference does.", CostUSD: 0.09},
			Prefer: judge.PreferB}
		switch {
		case n == 2:
			v.Prefer, v.BA, v.Errors = "", judge.PairOrder{}, []string{"exit 1, not JSON"}
		case n%5 == 1:
			v.Prefer, v.AB.Answer, v.BA.Answer = judge.PreferA, "first", "second"
			v.AB.Reason = "Change 1 keeps the parser's contract | Change 2 widens it; see /home/someone/.agentium/records/x with sk-ant-api03-" + // secret-scan: allow
				strings.Repeat("z", 40)
		case n%5 == 3:
			v.Prefer, v.Flip, v.AB.Answer, v.BA.Answer = judge.PreferTie, true, "first", "first"
		}
		return v
	})
	return e
}

// FewPairs is OneRun (an A/B, one run per arm) with the pair judge on: only three tasks passed in both arms, below the
// floor of judge.MinPreferences, and each comparison prefers lean's change.
func FewPairs() Experiment {
	e := OneRun(experiment.MethodV2, experiment.TemplateContextAB)
	e.Name = "lean-ab-few-pairs"
	e.Lock.Design.JudgePairs = &judge.Settings{Model: judge.DefaultModel, Effort: judge.DefaultEffort, Repeats: 1}
	comparePairs(&e, func(int, string) judge.PairVerdict {
		return judge.PairVerdict{Version: judge.PairVersion, Model: judge.DefaultModel, Prefer: judge.PreferB, CostUSD: 0.18,
			AB: judge.PairOrder{Answered: true, Answer: "second", Reason: "Change 2 is smaller."}, BA: judge.PairOrder{Answered: true, Answer: "first"}}
	})
	return e
}

// comparePairs stores verdict's comparison on the arm-B run of each pair of e whose two runs passed, numbering them
// from 0 in schedule order.
func comparePairs(e *Experiment, verdict func(n int, task string) judge.PairVerdict) {
	passed := map[int]string{} // pair → its arm-A run's ID
	for _, r := range e.Runs {
		if r.Record.Arm == "A" && r.Record.Passed != nil && *r.Record.Passed && r.Record.Outcome == claude.OutcomeOK {
			passed[e.Lock.Schedule[r.Slot].Pair] = r.ID
		}
	}
	n := 0
	for i := range e.Runs {
		r := &e.Runs[i]
		a, ok := passed[e.Lock.Schedule[r.Slot].Pair]
		if r.Record.Arm != "B" || !ok || r.Record.Passed == nil || !*r.Record.Passed || r.Record.Outcome != claude.OutcomeOK {
			continue
		}
		r.Record.PairJudge = &run.PairJudgement{RunA: a, Verdict: verdict(n, r.Record.Task)}
		n++
	}
}
