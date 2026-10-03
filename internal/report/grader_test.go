package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/claude"
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
	in.Runs[0].Record.Outcome, in.Runs[0].Record.Passed = claude.OutcomeInfra, nil
	in.Runs[1].Record.Grader, in.Runs[1].Record.Sandbox = task.GraderSandbox, &task.SandboxGrade{Canary: task.CanaryPassed, DenialCount: 2,
		FlaggedCount: 1, Denials: []sandbox.Denial{flagged}, Flagged: []sandbox.Denial{flagged}}
	in.Runs[1].Record.Outcome, in.Runs[1].Record.Passed = claude.OutcomeInfra, nil
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
				"1 failed grade(s) logged denials the agents' own sandbox does not impose: infrastructure, retried or left out; 1 passing grade(s) " +
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
