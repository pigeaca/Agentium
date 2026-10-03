package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/runner"
	"github.com/pigeaca/agentium/internal/sandbox"
)

func TestGraderModes(t *testing.T) {
	if GraderOf("") != GraderHost || GraderOf(GraderSandbox) != GraderSandbox {
		t.Error("GraderOf")
	}
	if DefaultGrader("darwin") != GraderSandbox || DefaultGrader("linux") != GraderHost {
		t.Error("DefaultGrader: the sandbox on macOS, the host elsewhere")
	}
	for flag, want := range map[string]string{"host": GraderHost, "sandbox": GraderSandbox, GraderSandbox: GraderSandbox} {
		if got, err := ParseGrader(flag); err != nil || got != want {
			t.Errorf("ParseGrader(%q) = %q, %v", flag, got, err)
		}
	}
	for _, flag := range []string{"", "Sandbox", "sandbox-v0", "docker"} {
		if _, err := ParseGrader(flag); err == nil {
			t.Errorf("ParseGrader(%q) accepted", flag)
		}
	}
	if !KnownGrader("") || !KnownGrader(GraderHost) || !KnownGrader(GraderSandbox) || KnownGrader("sandbox-v0") {
		t.Error("KnownGrader")
	}
}

// fakeSandbox is CheckoutCommands.Sandboxed run on the host: it moves the checkout into root (as the real one does),
// runs the commands there, removes root (bringing the checkout back with keep), and reports flagged denials when the
// command fails and flag is set, or the sandbox unavailable when down is set.
type fakeSandbox struct {
	flag, down bool
	roots      []string
}

func (f *fakeSandbox) run(ctx context.Context, dir, root string, keep bool, commands []string, timeout time.Duration, log io.Writer) ([]Command, bool, *SandboxGrade, error) {
	f.roots = append(f.roots, root)
	if f.down {
		return nil, false, &SandboxGrade{Canary: "the grading sandbox is unavailable: nested"}, fmt.Errorf("%w: nested", sandbox.ErrUnavailable)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, false, nil, err
	}
	copy := filepath.Join(root, "copy")
	if err := os.Rename(dir, copy); err != nil {
		return nil, false, nil, err
	}
	defer func() {
		if keep {
			os.Rename(copy, dir)
		}
		os.RemoveAll(root)
	}()
	var results []Command
	ok := true
	for _, c := range commands {
		r, err := runner.Run(ctx, runner.Spec{Dir: copy, Command: c, Timeout: timeout, Output: log})
		results = append(results, Command{Command: c, ExitCode: r.ExitCode})
		if err != nil {
			return results, false, nil, err
		}
		if !r.Passed() {
			ok = false
			break
		}
	}
	g := &SandboxGrade{Canary: CanaryPassed, Profile: "digest"}
	if f.flag && !ok {
		d := sandbox.Denial{Process: "java", Operation: "mach-lookup", Target: "com.apple.FontServer", Repeats: 1}
		g.DenialCount, g.FlaggedCount, g.Denials, g.Flagged = 1, 1, []sandbox.Denial{d}, []sandbox.Denial{d}
	}
	return results, ok, g, nil
}

// In sandbox mode each stage's verification runs through the sandboxed commands, in a grading folder of its own beside
// the checkouts, and the validation records the mode; host mode records "host" and never calls them. A stage that
// failed with denials the agent's sandbox does not impose is not OK, whatever it wanted; a sandbox that does not hold
// is an error, never a validation.
func TestValidateInTheSandbox(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	hidden, reference, err := Split(ctx, f.base, f.solution, "--git-dir", f.bare)
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{Base: f.base, Solution: f.solution, HiddenTests: hidden, Reference: reference, Verify: []string{"sh run_tests.sh"}}
	arms := []Arm{{Name: "base"}}
	with := func(fake *fakeSandbox, grader string) Validator {
		v, _ := validator(t, f.bare)
		v.Grader = grader
		v.Checkout = func(context.Context, string, []string, string) (CheckoutCommands, error) {
			return CheckoutCommands{Sandboxed: fake.run}, nil
		}
		return v
	}

	fake := &fakeSandbox{}
	v := with(fake, GraderSandbox)
	result, err := v.Validate(ctx, spec, arms)
	if err != nil || result.Status != StatusValid || result.Grader != GraderSandbox {
		t.Fatalf("in the sandbox: %s (grader %q), %v", result.Summary(), result.Grader, err)
	}
	if len(fake.roots) != 2 || filepath.Dir(fake.roots[0]) != filepath.Join(filepath.Dir(v.WorkDir), "grading") ||
		filepath.Base(fake.roots[0]) != "base-hidden-tests" || filepath.Base(fake.roots[1]) != "base-reference" {
		t.Errorf("grading roots %v: one per stage, beside the checkouts", fake.roots)
	}
	for _, s := range result.Stages {
		if s.Sandbox == nil || s.Sandbox.Canary != CanaryPassed {
			t.Errorf("stage %s/%s: sandbox %+v", s.Arm, s.Stage, s.Sandbox)
		}
	}
	if entries, _ := os.ReadDir(v.WorkDir); len(entries) != 0 {
		t.Errorf("checkouts left behind: %v", entries)
	}

	host := &fakeSandbox{}
	result, err = with(host, "").Validate(ctx, spec, arms)
	if err != nil || result.Status != StatusValid || result.Grader != GraderHost || len(host.roots) != 0 || result.Stages[0].Sandbox != nil {
		t.Errorf("on the host: %s (grader %q, sandbox calls %d), %v", result.Summary(), result.Grader, len(host.roots), err)
	}

	// The hidden-tests stage wants a failure, but a failure with flagged denials may be the sandbox's.
	flagged := &fakeSandbox{flag: true}
	v, progress := validator(t, f.bare)
	v.Grader, v.Checkout = GraderSandbox, func(context.Context, string, []string, string) (CheckoutCommands, error) {
		return CheckoutCommands{Sandboxed: flagged.run}, nil
	}
	result, err = v.Validate(ctx, spec, arms)
	if err != nil || result.Status != StatusInvalid || result.Stages[0].OK || len(result.Stages) != 1 ||
		!strings.Contains(result.Summary(), "base/hidden-tests failed with sandbox denials the agent's sandbox does not impose (mach-lookup)") ||
		!strings.Contains(progress.String(), "NOT OK (sandbox denials: mach-lookup)") {
		t.Errorf("flagged: %s, %v\n%s", result.Summary(), err, progress.String())
	}

	down := &fakeSandbox{down: true}
	if _, err := with(down, GraderSandbox).Validate(ctx, spec, arms); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Errorf("a sandbox that does not hold: %v", err)
	}

	// Keep: the checkout comes back from the grading folder.
	kept := &fakeSandbox{}
	v = with(kept, GraderSandbox)
	v.Keep = true
	if _, err := v.Validate(ctx, spec, arms); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(v.WorkDir); !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == "base-reference" }) {
		t.Errorf("kept checkouts: %v", entries)
	}

	// Sandbox mode needs the build tools' preparation to offer the sandboxed commands; an unknown mode is refused.
	v, _ = validator(t, f.bare)
	v.Grader = GraderSandbox
	if _, err := v.Validate(ctx, spec, arms); err == nil {
		t.Error("sandbox mode without Checkout validated")
	}
	v.Checkout = func(context.Context, string, []string, string) (CheckoutCommands, error) {
		return CheckoutCommands{}, nil
	}
	if _, err := v.Validate(ctx, spec, arms); err == nil {
		t.Error("sandbox mode without sandboxed commands validated")
	}
	v.Grader = "sandbox-v0"
	if _, err := v.Validate(ctx, spec, arms); err == nil {
		t.Error("an unknown grader validated")
	}
}
