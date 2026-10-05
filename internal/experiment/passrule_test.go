package experiment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/task"
)

// An experiment locked before the proof keeps the exit codes: its lock (a stored report's, from before) reads with no
// rule, passes the resume check, has every run graded by the exit codes alone, and encodes as it did, so what reads it
// (the analysis, the reports, whose goldens are built from such locks) sees nothing new.
func TestAnOldLockKeepsTheExitCodes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "report", "testdata", "lean-ab.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Lock json.RawMessage `json:"lock"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored.Lock), "pass_rule") {
		t.Fatal("the fixture is not a lock from before the proof")
	}
	var l Lock
	if err := json.Unmarshal(stored.Lock, &l); err != nil {
		t.Fatal(err)
	}
	if l.PassRule() != task.PassExitCode || l.Design.PassRule != "" {
		t.Errorf("rule %q, design's %q", l.PassRule(), l.Design.PassRule)
	}
	if err := l.Check(l.ClaudeCode, l.SignIn); err != nil {
		t.Errorf("resume check: %v", err)
	}
	if env := lockedRunEnv(run.Env{}, l); !env.ExitCodeOnly || env.Grader != task.GraderOf(l.Grader) {
		t.Errorf("its runs: exit codes only %v, grader %q", env.ExitCodeOnly, env.Grader)
	}
	if encoded, _ := json.Marshal(l); strings.Contains(string(encoded), "pass_rule") {
		t.Error("an old lock gained a rule when encoded")
	}
}

// A design with a task whose hidden tests include Go tests records the rule, and its lock fixes it: the runs need the
// proof. A lock under a rule this Agentium does not know is refused.
func TestANewLockRecordsTheRule(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	proj, err := db.SaveProject(ctx, "/repo", "repo", []byte(`{}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for name, hidden := range map[string]string{"go": "a_test.go", "shell": "tests/a_test.sh"} {
		if _, err := db.SaveTask(ctx, store.Task{ProjectID: proj.ID, Name: name, Instruction: "Do it.", Source: "manual", BaseCommit: "b",
			SolutionCommit: "s", HiddenTests: []string{hidden}, Reference: []string{"x"}, Verify: []string{"true"}}); err != nil {
			t.Fatal(err)
		}
	}
	p := Project{DB: db, ID: proj.ID}
	d := validDesign()
	d.Tasks = []string{"shell"}
	if err := p.markPassRule(ctx, &d); err != nil || d.PassRule != "" || d.Version != DesignVersion {
		t.Errorf("no Go tests: rule %q, version %d, %v", d.PassRule, d.Version, err)
	}
	d.Tasks = []string{"go", "shell"}
	if err := p.markPassRule(ctx, &d); err != nil || d.PassRule != task.PassGoTests || d.Version != DesignVersionProof || d.Validate() != nil {
		t.Fatalf("Go tests: rule %q, version %d, %v, %v", d.PassRule, d.Version, err, d.Validate())
	}
	d.Tasks = []string{"go"}
	if err := db.SaveCalibration(ctx, store.Calibration{ProjectID: proj.ID, Arm: BaseContext, RunID: "cal",
		Result: []byte(`{"requested_model":"` + d.Model + `"}`), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d.Template, d.Arms = TemplateAA, []Arm{{Name: "A", Context: BaseContext}, {Name: "B", Context: BaseContext}}
	l, err := Runner{Project: p, Now: time.Now}.buildLock(ctx, d, "claude", "2.1.281")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(l)
	if l.PassRule() != task.PassGoTests || !strings.Contains(string(encoded), `"pass_rule":"go-tests-v1"`) {
		t.Errorf("the lock's rule %q:\n%s", l.PassRule(), encoded)
	}
	if env := lockedRunEnv(run.Env{}, l); env.ExitCodeOnly {
		t.Error("a lock with the rule grades by the exit codes")
	}
	l = Lock{Method: MethodV2, ClaudeCode: "2.1.281", SignIn: "login", Pass: "go-tests-v9"}
	if err := l.Check("2.1.281", "login"); err == nil || !strings.Contains(err.Error(), "go-tests-v9") || !strings.Contains(err.Error(), "start a new experiment") {
		t.Errorf("an unknown rule: %v", err)
	}
}

// A design with the rule is stored under its own version, whatever else it is: an older Agentium, which does not know
// the field, reads the design without it, finds the version is not the one it would write, and refuses to run, resume
// or report it (Project.Load) rather than grade by the exit codes.
func TestTheRulesDesignVersionIsRefusedByAnOlderAgentium(t *testing.T) {
	for name, edit := range map[string]func(*Design){
		"host":    func(*Design) {},
		"sandbox": func(d *Design) { d.Grader = task.GraderSandbox },
		"seq-v1":  func(d *Design) { d.Method, d.Repeats, d.Goal = MethodSeq, 1, GoalCheaper },
	} {
		d := validDesign()
		edit(&d)
		d.PassRule = task.PassGoTests
		d.Version = d.WantVersion()
		if d.Version != DesignVersionProof || d.Validate() != nil {
			t.Errorf("%s: version %d, %v", name, d.Version, d.Validate())
		}
		encoded, _ := json.Marshal(d)
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, "pass_rule") // what an older Agentium's Design keeps of it
		older, _ := json.Marshal(fields)
		var seen Design
		if err := json.Unmarshal(older, &seen); err != nil {
			t.Fatal(err)
		}
		if seen.Version == seen.WantVersion() {
			t.Errorf("%s: an older Agentium would read version %d as its own", name, seen.Version)
		}
	}
	d := validDesign()
	d.PassRule = task.PassExitCode // a design records the proof's rule or none
	d.Version = d.WantVersion()
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "pass rule") {
		t.Errorf("a design that names the exit codes: %v", err)
	}
}

// When an experiment that grades by the proof locks, a task with hidden Go tests validated before the proof is
// validated again in the experiment's mode (time, no money); one validated with it, one without Go tests and a judge-
// graded one are not. An experiment without the rule validates none again for it. The mode's own case is unchanged.
func TestRevalidationReason(t *testing.T) {
	valid := func(grader, rule string) *task.Validation {
		return &task.Validation{Status: task.StatusValid, Grader: grader, PassRule: rule}
	}
	for name, c := range map[string]struct {
		cand         Candidate
		grader, rule string
		want         string
	}{
		"host, Go, before the proof":    {Candidate{GoTests: true, Validation: valid("", "")}, task.GraderHost, task.PassGoTests, RevalidateProof},
		"host, Go, with the proof":      {Candidate{GoTests: true, Validation: valid(task.GraderHost, task.PassGoTests)}, task.GraderHost, task.PassGoTests, ""},
		"host, not Go":                  {Candidate{Validation: valid("", "")}, task.GraderHost, task.PassGoTests, ""},
		"host, Go, an old experiment":   {Candidate{GoTests: true, Validation: valid("", "")}, task.GraderHost, "", ""},
		"host, validated in a sandbox":  {Candidate{GoTests: true, Validation: valid(task.GraderSandbox, "")}, task.GraderHost, task.PassGoTests, ""},
		"sandbox, Go, before the proof": {Candidate{GoTests: true, Validation: valid(task.GraderSandbox, "")}, task.GraderSandbox, task.PassGoTests, RevalidateProof},
		"sandbox, validated on host":    {Candidate{GoTests: true, Validation: valid("", "")}, task.GraderSandbox, task.PassGoTests, RevalidateMode},
		"sandbox, old, on host":         {Candidate{Validation: valid("", "")}, task.GraderSandbox, "", RevalidateMode},
		"judge-graded":                  {Candidate{GoTests: true, Grading: task.GradingJudge, Validation: valid("", "")}, task.GraderHost, task.PassGoTests, ""},
		"never validated":               {Candidate{GoTests: true}, task.GraderHost, task.PassGoTests, ""},
	} {
		if got := RevalidationReason(c.cand, c.grader, c.rule); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
		if NeedsRevalidation(c.cand, c.grader, c.rule) != (c.want != "") {
			t.Errorf("%s: NeedsRevalidation disagrees", name)
		}
	}
}
