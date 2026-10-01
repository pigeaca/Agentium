package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// flip passes on one call and fails on the next, using a marker outside every checkout (each stage has a fresh one).
func flip(marker string) string {
	return fmt.Sprintf("if [ -e %[1]s ]; then rm %[1]s; exit 1; else touch %[1]s; fi", marker)
}

func TestRepeatFindsFlakyStages(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	marker := filepath.Join(t.TempDir(), "marker")
	arms := []Arm{{Name: "base"}}

	for _, c := range []struct {
		repeats int
		passed  int
	}{{2, 1}, {3, 2}} {
		os.Remove(marker)
		v, progress := validator(t, f.bare)
		v.Repeats = c.repeats
		got, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{flip(marker)}}, arms)
		if err != nil {
			t.Fatal(err)
		}
		s := got.Stages[0]
		if got.Status != StatusFlaky || !s.Flaky || s.OK || s.Runs != c.repeats || s.PassedRuns != c.passed || got.Repeats != c.repeats {
			t.Errorf("repeat %d: status %s, stage %+v", c.repeats, got.Status, s)
		}
		want := fmt.Sprintf("flaky: base/base passed %d of %d times", c.passed, c.repeats)
		if got.Summary() != want {
			t.Errorf("summary %q, want %q", got.Summary(), want)
		}
		if p := progress.String(); !strings.Contains(p, fmt.Sprintf("(1/%d)", c.repeats)) || !strings.Contains(p, fmt.Sprintf("(%d/%d)", c.repeats, c.repeats)) ||
			!strings.Contains(p, want) {
			t.Errorf("progress:\n%s", p)
		}
		for i := 1; i <= c.repeats; i++ { // every repeat has its own log
			if _, err := os.Stat(filepath.Join(v.LogDir, fmt.Sprintf("base-base-r%d.log", i))); err != nil {
				t.Errorf("repeat %d: %v", i, err)
			}
		}
	}

	// One run keeps today's behavior and stored form: valid-as-checked or invalid, nothing about repeats.
	v, progress := validator(t, f.bare)
	os.Remove(marker)
	one, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{flip(marker)}}, arms)
	if err != nil || one.Status != StatusUnchecked || one.Stages[0].Runs != 0 || one.Repeats != 0 || strings.Contains(progress.String(), "/") {
		t.Errorf("one run: %+v, %v\n%s", one, err, progress.String())
	}
	if raw, _ := json.Marshal(one); strings.Contains(string(raw), "repeats") || strings.Contains(string(raw), "runs") || strings.Contains(string(raw), "flaky") {
		t.Errorf("a single run stores new fields: %s", raw)
	}
	two, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{flip(marker)}}, arms) // the marker is now set
	if err != nil || two.Status != StatusInvalid {
		t.Errorf("one failing run: %s, %v", two.Status, err)
	}

	// Runs that agree are not flaky, whatever the count.
	v.Repeats = 3
	steady, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{"true"}}, arms)
	if err != nil || steady.Status != StatusUnchecked || steady.Stages[0].OKRuns != 3 || steady.Stages[0].Flaky {
		t.Errorf("steady: %+v, %v", steady, err)
	}
	broken, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{"false"}}, arms)
	if err != nil || broken.Status != StatusInvalid || broken.Stages[0].Flaky || broken.Stages[0].OKRuns != 0 {
		t.Errorf("always failing: %+v, %v", broken, err)
	}
}

func TestRepeatWithFlakySetupAndHiddenTests(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	marker := filepath.Join(t.TempDir(), "marker")
	v, _ := validator(t, f.bare)
	v.Repeats = 2
	// Setup that fails on one repeat only: the runs disagree, so the stage is flaky and says why.
	got, err := v.Validate(ctx, Spec{Base: f.base, Setup: []string{flip(marker)}, Verify: []string{"true"}}, []Arm{{Name: "base"}})
	if err != nil || got.Status != StatusFlaky || got.Stages[0].SetupFailedRuns != 1 || !strings.Contains(got.Summary(), "its setup failed 1 times") {
		t.Errorf("flaky setup: %s, %+v, %v", got.Summary(), got.Stages, err)
	}
	// Hidden tests must fail: one failing run and one passing run disagree even though one of them is "right".
	os.Remove(marker)
	got, err = v.Validate(ctx, Spec{Base: f.base, Solution: f.solution, HiddenTests: []string{"tests/value_test.sh"},
		Reference: []string{"value.txt"}, Verify: []string{flip(marker)}}, []Arm{{Name: "base"}})
	if err != nil || got.Status != StatusFlaky || len(got.Stages) != 1 || got.Stages[0].Stage != StageHiddenTests {
		t.Errorf("flaky hidden tests: %s, %v", stages(got), err)
	}
}

func TestRepeatStopsWhenCancelled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v, _ := validator(t, f.bare)
	v.Repeats = 3
	v.Started = func(arm, stage string) {
		if stage == "base 2/3" {
			cancel()
		}
	}
	got, err := v.Validate(ctx, Spec{Base: f.base, Verify: []string{"true"}}, []Arm{{Name: "base"}})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation", err)
	}
	if got.Status == StatusFlaky || len(got.Stages) != 1 || got.Stages[0].Flaky || got.Stages[0].OK {
		t.Errorf("a cancelled repeat is no verdict: %s %+v", got.Status, got.Stages)
	}
	if entries, _ := os.ReadDir(v.WorkDir); len(entries) != 0 {
		t.Errorf("checkouts left behind: %v", entries)
	}
}

func TestStoredValidationWithoutRepeatsMeansOne(t *testing.T) {
	var v Validation
	old := `{"status":"valid","arms":[{"name":"base"}],"stages":[{"arm":"base","stage":"reference","want":"pass","passed":true,"ok":true,"commands":[],"log":"x"}],"at":"2026-09-28T12:00:00Z"}`
	if err := json.Unmarshal([]byte(old), &v); err != nil {
		t.Fatal(err)
	}
	if v.RepeatCount() != 1 || v.Stages[0].Flaky || v.Summary() != "valid" {
		t.Errorf("old validation: repeats %d, %+v, %q", v.RepeatCount(), v.Stages, v.Summary())
	}
	if (Validation{Repeats: 3}).RepeatCount() != 3 {
		t.Error("repeats not read")
	}
}
