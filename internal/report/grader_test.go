package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/sandbox"
	"github.com/pigeaca/agentium/internal/task"
	"github.com/pigeaca/agentium/internal/term"
)

// The report says where the runs were graded, in every format, and what the grading sandbox did to them: grades
// that did not run (the canary), failed grades with denials the agents' sandbox does not impose (both left out as
// infrastructure) and passing grades with such denials. What the grade chose (a denial's target) never reaches it. A
// lock from before modes says nothing new.
func TestReportShowsTheGrader(t *testing.T) {
	in := fixture()
	in.Lock.Grader = task.GraderSandbox
	flagged := sandbox.Denial{Process: "java", Operation: "mach-lookup", Target: "com.apple.SecretLeakingName", Repeats: 1}
	in.Runs[0].Record.Grader, in.Runs[0].Record.Sandbox = task.GraderSandbox, &task.SandboxGrade{Canary: "the grading sandbox is unavailable: nested"}
	in.Runs[0].Record.Outcome, in.Runs[0].Record.Passed = agent.OutcomeInfra, nil
	in.Runs[1].Record.Grader, in.Runs[1].Record.Sandbox = task.GraderSandbox, &task.SandboxGrade{Canary: task.CanaryPassed, DenialCount: 2,
		FlaggedCount: 1, Denials: []sandbox.Denial{flagged}, Flagged: []sandbox.Denial{flagged}}
	in.Runs[1].Record.Outcome, in.Runs[1].Record.Passed = run.OutcomeSandboxFlagged, nil
	passed := true
	in.Runs[2].Record.Grader, in.Runs[2].Record.Sandbox = task.GraderSandbox, &task.SandboxGrade{Canary: task.CanaryPassed, DenialCount: 1,
		FlaggedCount: 1, Flagged: []sandbox.Denial{flagged}}
	in.Runs[2].Record.Passed = &passed
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var md, txt, js bytes.Buffer
	rep.Markdown(&md)
	rep.Terminal(&txt, term.Style{})
	rep.JSON(&js)
	for name, out := range map[string]string{"Markdown": md.String(), "terminal": txt.String(), "JSON": js.String()} {
		for _, want := range []string{"Graded in Agentium's grading sandbox (sandbox-v1)", "localhost is every address of the machine",
			"Grading sandbox: 1 grade(s) did not run because the sandbox did not hold (its canary failed): infrastructure, retried or left out; " +
				"failed grades with denials the agents' own sandbox does not impose were left out, not tried again (A ", "1 passing grade(s) " +
				"logged such denials and stay passes."} {
			if name != "JSON" && !strings.Contains(out, want) || name == "JSON" && strings.HasPrefix(want, "Grading sandbox") && !strings.Contains(out, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if strings.Contains(out, "SecretLeakingName") {
			t.Errorf("%s names a denial's target", name)
		}
	}
	for _, want := range []string{`"grader": "sandbox-v1"`, `"canary": "failed"`, `"flagged_operations": [`} {
		if !strings.Contains(js.String(), want) {
			t.Errorf("the JSON lacks %s", want)
		}
	}

	host := fixture()
	host.Lock.Grader = task.GraderHost
	rep, _ = Build(host)
	md.Reset()
	rep.Markdown(&md)
	if !strings.Contains(md.String(), "Graded on the host, without a sandbox") || strings.Contains(md.String(), "Grading sandbox:") {
		t.Errorf("a host lock's report:\n%s", md.String())
	}
	rep, _ = Build(fixture())
	md.Reset()
	rep.Markdown(&md)
	if strings.Contains(md.String(), "Graded") {
		t.Error("a lock from before modes names one")
	}
}

// Runs left out for flagged sandbox denials are counted per arm in every format; when the arms differ, the verdicts
// are demoted to inconclusive and the report says what counting them as fails gives. Balanced arms keep their verdicts.
func TestReportShowsFlaggedExclusionsPerArm(t *testing.T) {
	in := fixture()
	in.Lock.Grader = task.GraderSandbox
	b := -1
	for i, r := range in.Runs {
		if r.Record.Arm == "B" && r.Record.Outcome == agent.OutcomeOK {
			b = i
			break
		}
	}
	in.Runs[b].Record.Outcome, in.Runs[b].Record.Passed = run.OutcomeSandboxFlagged, nil
	in.Runs[b].Record.Grader, in.Runs[b].Record.Sandbox = task.GraderSandbox, &task.SandboxGrade{Canary: task.CanaryPassed, FlaggedCount: 1,
		Flagged: []sandbox.Denial{{Process: "java", Operation: "mach-lookup", Target: "com.apple.FontServer", Repeats: 1}}}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var md, txt, js bytes.Buffer
	rep.Markdown(&md)
	rep.Terminal(&txt, term.Style{})
	rep.JSON(&js)
	for name, out := range map[string]string{"Markdown": md.String(), "terminal": txt.String(), "JSON": js.String()} {
		for _, want := range []string{"were left out, not tried again (A 0, B 1;", "counting them as fails gives cost ",
			"the arms differ, so the cost and success verdicts are demoted to inconclusive",
			"1 left out for sandbox denials (not tried again)"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(md.String(), "demoted to inconclusive: the arms' runs left out for sandbox denials differ (A 0, B 1)") {
		t.Error("the headline does not say why the verdict is inconclusive")
	}
	if !strings.Contains(js.String(), `"imbalanced": true`) || !strings.Contains(js.String(), `"as_fails"`) {
		t.Error("the JSON analysis lacks the sandbox check")
	}
}

// The lock's harmless sandbox denials name paths under the user's home and text a grade chose: the report shares a
// count per task, in every format, never the denials.
func TestReportSharesNoHarmlessDenial(t *testing.T) {
	in := fixture()
	in.Lock.Grader = task.GraderSandbox
	name := in.Lock.Tasks[0].Name
	in.Lock.Harmless = map[string][]task.DenialKey{name: {{Operation: "file-read-data", Target: "/Users/someone/.m2/settings.xml"},
		{Operation: "mach-lookup", Target: "com.apple.SecretLeakingName"}}}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var md, txt, js bytes.Buffer
	rep.Markdown(&md)
	rep.Terminal(&txt, term.Style{})
	rep.JSON(&js)
	for format, out := range map[string]string{"Markdown": md.String(), "terminal": txt.String(), "JSON": js.String()} {
		if strings.Contains(out, "SecretLeakingName") || strings.Contains(out, "settings.xml") || strings.Contains(out, "/Users/someone") {
			t.Errorf("%s shares a harmless denial", format)
		}
	}
	if !strings.Contains(js.String(), "(2 harmless denials)") {
		t.Error("the JSON lock lacks the per-task count")
	}
	if got := in.Lock.Harmless[name][1].Target; got != "com.apple.SecretLeakingName" {
		t.Error("the input's lock was changed")
	}
}
