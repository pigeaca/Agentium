package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/agent"
	"github.com/pigeaca/agentium/internal/claude"
	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/report/reporttest"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

// panelScene is a review with what plan draws beside it: the cautions and the sign-in runs would use.
type panelScene struct {
	review   experiment.Review
	cautions planCautions
	signIn   string
}

func panelTasks(n int) []string {
	var out []string
	for i := range n {
		out = append(out, fmt.Sprintf("task-%02d", i))
	}
	return out
}

// usageRun is a task run that read the five-hour window: from 0.45 to used, the window resetting two hours on.
func usageRun(used float64) store.Run {
	reading := func(v float64) agent.UsageReading {
		return agent.UsageReading{FiveHour: v, FiveHourResets: planNow.Add(2 * time.Hour), Status: "allowed"}
	}
	rec, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5", "metrics": map[string]any{"usage_first": reading(0.45), "usage_last": reading(used), "saw_result": true}})
	return store.Run{Kind: "task", Outcome: agent.OutcomeOK, Record: rec, Started: planNow.Add(-40 * time.Minute), Finished: planNow.Add(-30 * time.Minute)}
}

// panelScenes are the preview with each state of the new panels. The estimate is $1.23 a run, 38 requests a run and the
// contexts 20,000 and 15,425 tokens (4,575 fewer) unless a scene says otherwise: the plan's check.
func panelScenes(t *testing.T) map[string]panelScene {
	t.Helper()
	ready := experiment.Readiness{Ready: true, Checks: []experiment.Check{{Status: "ok", Text: "Claude Code 2.1.281"}, {Status: "ok", Text: "8 task(s), each valid in every arm's context"}}}
	build := func(method, goal string, tasks, repeats int) experiment.Review {
		d := reporttest.OneRun(method, experiment.TemplateContextAB).Lock.Design
		d.Method, d.Goal, d.Tasks, d.Repeats = method, goal, panelTasks(tasks), repeats
		var past []experiment.PastRun
		for _, task := range d.Tasks[:min(3, len(d.Tasks))] {
			past = append(past, experiment.PastRun{Task: task, CostUSD: 1.23})
		}
		est := experiment.EstimateRun(d.Model, past)
		est.CapUSD = d.RunBudgetUSD
		return experiment.Review{Design: d, Estimates: experiment.Same(est), Rows: experiment.PreviewFor(d, d.Tasks, experiment.Same(est)), Readiness: ready,
			Size: experiment.ContextSize{FirstRequest: [2]int64{20000, 15425}, Requests: 38, PastRuns: 6}}
	}
	unsure := build(experiment.MethodV2, experiment.GoalCheaper, 8, 1)
	sees := unsure // a $0.15 run: the same 4,575 tokens are 35% of it
	cheap := experiment.EstimateRun(sees.Design.Model, []experiment.PastRun{{Task: "task-00", CostUSD: 0.15}, {Task: "task-01", CostUSD: 0.15}, {Task: "task-02", CostUSD: 0.15}})
	cheap.CapUSD = sees.Design.RunBudgetUSD
	sees.Estimates = experiment.Same(cheap)
	nosize := unsure
	nosize.Size = experiment.ContextSize{}
	model := build(experiment.MethodV2, experiment.GoalCheaper, 8, 1)
	model.Design.Template = experiment.TemplateModelAB
	model.Design.Arms[0].Model, model.Design.Arms[1].Model = "claude-sonnet-5", "claude-haiku-4-5"
	model.Size = unsure.Size
	aa := unsure
	aa.Design.Template = experiment.TemplateAA
	below := build(experiment.MethodV2, experiment.GoalBetter, 12, 3)
	above := build(experiment.MethodV2, experiment.GoalBetter, 20, 3)
	seq := build(experiment.MethodSeq, experiment.GoalCheaper, 16, 1)
	some, all := unsure, build(experiment.MethodV2, experiment.GoalCheaper, 6, 1) // 12 runs fit in what is left of the window
	some.Runs = []store.Run{usageRun(0.51)}
	all.Runs = []store.Run{usageRun(0.02)}
	tiny := unsure // 4 tokens fewer: about 0.00004% of a run
	tiny.Size.FirstRequest = [2]int64{20000, 19996}
	return map[string]panelScene{
		"ctx-unsure":    {review: unsure, signIn: claude.SignInLogin},
		"ctx-sees":      {review: sees, signIn: claude.SignInLogin},
		"ctx-nosize":    {review: nosize, signIn: claude.SignInLogin},
		"ctx-seq":       {review: seq, signIn: claude.SignInLogin},
		"model":         {review: model, signIn: claude.SignInLogin},
		"aa":            {review: aa, signIn: claude.SignInLogin},
		"better-below":  {review: below, signIn: claude.SignInLogin},
		"better-above":  {review: above, signIn: claude.SignInLogin},
		"usage-some":    {review: some, signIn: claude.SignInLogin},
		"usage-all":     {review: all, signIn: claude.SignInLogin},
		"api-key":       {review: some, signIn: claude.SignInAPIKey},
		"cautions":      {review: above, signIn: claude.SignInLogin, cautions: planCautions{Applies: true, TooEasy: []string{"parse-dates", "retry-http", "rename-flag"}, NeverPassed: []string{"cache-race", "tz-shift"}}},
		"cautions-long": {review: above, signIn: claude.SignInLogin, cautions: planCautions{Applies: true, TooEasy: panelTasks(14), NeverPassed: []string{"only-one"}}},
		"tiny":          {review: tiny, signIn: claude.SignInLogin},
	}
}

var panelSceneNames = []string{"ctx-unsure", "ctx-sees", "ctx-nosize", "ctx-seq", "model", "aa", "better-below", "better-above", "usage-some", "usage-all", "api-key", "cautions", "cautions-long", "tiny"}

func TestPlanPanelGoldens(t *testing.T) {
	scenes := panelScenes(t)
	view := func(name string, sh term.Shapes, width int) []string {
		s := scenes[name]
		return planViewWith(s.review, s.cautions, "lean-ab", s.signIn, planNow, sh, width)
	}
	for _, name := range panelSceneNames {
		var b strings.Builder
		for _, width := range []int{term.MinWidth, 80, 120} {
			fmt.Fprintf(&b, "=== %d columns\n", width)
			for _, line := range view(name, plainUnicode, width) {
				if w := term.Width(line); w > width {
					t.Errorf("%s at %d columns: a line of %d cells: %q", name, width, w, line)
				}
				b.WriteString(line + "\n")
			}
		}
		checkGolden(t, "plan-panels-"+name+".golden", b.String())
	}
	for style, sh := range map[string]term.Shapes{"color": color256, "ascii": plainASCII} {
		var b strings.Builder
		for _, name := range panelSceneNames {
			fmt.Fprintf(&b, "=== %s\n", name)
			b.WriteString(strings.Join(view(name, sh, 80), "\n") + "\n")
		}
		checkGolden(t, "plan-panels-"+style+".golden", b.String())
	}
}

func TestPlanPanelWords(t *testing.T) {
	scenes := panelScenes(t)
	// text is the view with its frames taken off and its lines joined, so a phrase the panel wrapped is still one.
	text := func(name string) string {
		s := scenes[name]
		var out []string
		for _, line := range planViewWith(s.review, s.cautions, "lean-ab", s.signIn, planNow, plainUnicode, 100) {
			out = append(out, strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "│")))
		}
		return strings.Join(out, " ")
	}
	want := map[string][]string{
		"ctx-unsure":    {"can it answer?", "a cost change of 25% or more", "about 4% less: the context is 4,575 tokens smaller", "not sure", "seeing 4% would take about 690 runs", "size is not everything"},
		"ctx-sees":      {"a cost change of 25% or more", "about 35% less: the context is 4,575 tokens smaller"},
		"better-below":  {"no answer on passes at this size: it needs 20 tasks of 3 runs each"},
		"better-above":  {"a pass-rate change of"},
		"usage-some":    {"your plan", "five-hour limit", "51% used", "runs pause at 85%", "more runs fit now", "add --wait"},
		"usage-all":     {"your plan", "every run fits now"},
		"cautions":      {"3 tasks passed every time so far, so they may not tell the versions apart: parse-dates, retry-http, rename-flag", "2 tasks never passed: check their text (agentium task show NAME): cache-race, tz-shift"},
		"cautions-long": {"1 task never passed: check its text", "more: agentium task list"},
		"tiny":          {"under 0.1% less: the context is 4 tokens smaller", "seeing under 0.1% would take more than 10,000 runs"},
	}
	for name, words := range want {
		v := text(name)
		for _, w := range words {
			if !strings.Contains(v, w) {
				t.Errorf("%s: want %q in:\n%s", name, w, v)
			}
		}
	}
	absent := map[string][]string{
		"ctx-sees":     {"not sure", "seeing "},
		"ctx-nosize":   {"expected from size", "likely result"},
		"model":        {"expected from size", "likely result", "not everything"},
		"aa":           {"expected from size", "likely result"},
		"better-above": {"expected from size", "passed every time"},
		"usage-some":   {"on your plan:"},
		"api-key":      {"╭─ your plan", "on your plan"},
		"ctx-unsure":   {"╭─ your plan", "passed every time"},
	}
	for name, words := range absent {
		v := text(name)
		for _, w := range words {
			if strings.Contains(v, w) {
				t.Errorf("%s: has %q:\n%s", name, w, v)
			}
		}
	}
	// No reading: today's line stays exactly as it is.
	if v := text("ctx-unsure"); !strings.Contains(v, "on your plan: 16 runs need about") {
		t.Errorf("without a current reading the dim line is gone:\n%s", v)
	}
	// Cautions are not "not ready", and a long list is cut to two lines.
	if v := text("cautions-long"); strings.Contains(v, "not ready") || strings.Contains(v, "task-13") {
		t.Errorf("cautions-long:\n%s", v)
	}
}

func TestPlanPanelsNeverBlock(t *testing.T) {
	for name, s := range panelScenes(t) {
		if v := strings.Join(planViewWith(s.review, s.cautions, "lean-ab", s.signIn, planNow, plainUnicode, 80), "\n"); strings.Contains(v, "not ready to run") {
			t.Errorf("%s: a panel made the plan not ready:\n%s", name, v)
		}
	}
}

// Cautions are advice: a store that cannot be read gives none, and no failure.
func TestPlanCautionsOfAnUnreadableStore(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "agentium.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	c := planCautionsOf(ctx, &workspace{db: db}, experiment.Design{Goal: experiment.GoalBetter, Tasks: []string{"a"}})
	if c.Applies || len(c.TooEasy) != 0 || len(c.NeverPassed) != 0 {
		t.Errorf("%+v", c)
	}
}

// An unreadable calibration is an unknown size: the preview is printed, exit 0, with no expected change.
func TestPlanSurvivesAnUnreadableCalibration(t *testing.T) {
	t.Parallel()
	f, _ := seqFixture(t)
	jsonRun(t, f, ExitOK, "experiment", "new", "size", "--b", "lean", "--seed", "5")
	seedSizeFacts(t, f, "claude-sonnet-5-5")
	ctx := context.Background()
	db, id := openFixtureDB(t, f)
	latest, err := db.LatestCalibration(ctx, id, "base", "")
	if err != nil {
		t.Fatal(err)
	}
	latest.ID, latest.Result, latest.CreatedAt = 0, []byte("not json"), latest.CreatedAt.Add(time.Hour)
	if err := db.SaveCalibration(ctx, latest); err != nil {
		t.Fatal(err)
	}
	plan := jsonRun(t, f, ExitOK, "experiment", "plan", "size")
	if plan.get("can_answer", "expected_change") != nil || plan.get("can_answer", "metric") != "cost" {
		t.Errorf("an unreadable calibration: %s", plan.stdout)
	}
	*f.terminal = true
	expect(t, f.run(ctx, "experiment", "plan", "size"), ExitOK, "can it answer?")
}

func TestGroupDigits(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 4575: "4,575", 1234567: "1,234,567", -4575: "-4,575"} {
		if got := groupDigits(n); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", n, got, want)
		}
	}
}

// can_answerKeys and expectedChangeKeys are the new keys of experiment plan --json.
const (
	canAnswerKeys      = "expected_change,floor_met,floor_repeats,floor_tasks,likely_result_not_sure,metric,smallest_change"
	expectedChangeKeys = "change,requests_per_run,run_usd,runs_to_see,tokens_a,tokens_b"
)

// seedSizeFacts gives the fixture's two contexts calibrations of 20,000 and 15,425 tokens (4,575 fewer in lean) and
// three earlier task runs of 38 turns at $1.23 on the experiment's model: the plan's check.
func seedSizeFacts(t *testing.T, f runFixture, model string) {
	t.Helper()
	ctx := context.Background()
	db, id := openFixtureDB(t, f)
	snap, err := db.SnapshotByName(ctx, id, "lean")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		arm, snapshot string
		tokens        int64
	}{{"base", "", 20000}, {"lean", snap.CommitID, 15425}} {
		latest, err := db.LatestCalibration(ctx, id, c.arm, c.snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(latest.Result, &result); err != nil {
			t.Fatal(err)
		}
		result["first_request_tokens"] = c.tokens
		latest.Result, _ = json.Marshal(result)
		latest.ID, latest.CreatedAt = 0, latest.CreatedAt.Add(time.Minute)
		if err := db.SaveCalibration(ctx, latest); err != nil {
			t.Fatal(err)
		}
	}
	rec := fmt.Sprintf(`{"model":%q,"effort_recorded":true,"metrics":{"saw_result":true,"turns":38}}`, model)
	var runs []store.Run
	for range 3 {
		runs = append(runs, store.Run{Kind: "task", Outcome: agent.OutcomeOK, CostUSD: 1.23, Record: []byte(rec)})
	}
	saveRuns(t, f, runs...)
}

// experiment plan --json carries the numbers the screen shows, for the plan's check: 4,575 fewer tokens, 38 requests a
// run and $1.23 a run give about 4%, which a design this size cannot see: about 690 runs would.
func TestPlanJSONCarriesWhatTheScreenShows(t *testing.T) {
	t.Parallel()
	f, _ := seqFixture(t)
	jsonRun(t, f, ExitOK, "experiment", "new", "size", "--b", "lean", "--seed", "5")
	seedSizeFacts(t, f, "claude-sonnet-5-5")

	plan := jsonRun(t, f, ExitOK, "experiment", "plan", "size")
	assertKeys(t, plan.doc, planKeys)
	assertKeys(t, plan.get("can_answer"), canAnswerKeys)
	assertKeys(t, plan.get("can_answer", "expected_change"), expectedChangeKeys)
	got := plan.get("can_answer").(map[string]any)
	expected := plan.get("can_answer", "expected_change").(map[string]any)
	smallest, _ := got["smallest_change"].(float64)
	change, _ := expected["change"].(float64)
	if got["metric"] != "cost" || got["floor_met"] != true || got["floor_tasks"] != float64(8) || got["floor_repeats"] != float64(1) ||
		got["likely_result_not_sure"] != true || smallest < 0.1 || smallest > 0.3 {
		t.Errorf("can_answer: %s", plan.stdout)
	}
	if math.Abs(change-0.052155/1.23) > 1e-6 || expected["tokens_a"] != float64(20000) || expected["tokens_b"] != float64(15425) ||
		expected["requests_per_run"] != float64(38) || math.Abs(expected["run_usd"].(float64)-1.23) > 1e-9 || expected["runs_to_see"] != float64(690) {
		t.Errorf("expected_change: %s", plan.stdout)
	}
	if plan.get("tasks_passed_every_time") != nil || plan.get("tasks_never_passed") != nil {
		t.Errorf("a cost experiment has cautions about tasks: %s", plan.stdout)
	}

	*f.terminal = true
	screen := term.Plain(f.run(context.Background(), "experiment", "plan", "size").stdout)
	for _, want := range []string{"can it answer?", fmt.Sprintf("a cost change of %.0f%% or more", 100*smallest), "about 4% less: the context is 4,575 tokens smaller", "not sure",
		"seeing 4% would take about 690 runs"} {
		if !strings.Contains(flat(screen), want) {
			t.Errorf("the screen lacks %q:\n%s", want, screen)
		}
	}
	// A plain review (a pipe, --details) has none of the new panels.
	*f.terminal = false
	if plain := f.run(context.Background(), "experiment", "plan", "size").stdout; strings.Contains(plain, "can it answer") {
		t.Errorf("the plain review has the panel:\n%s", plain)
	}
}

// For --goal better, the plan names the tasks that passed every time (4 or more graded runs) and those that never
// passed (2 or more), as the task list tags them; JSON has them as two lists, null for another goal.
func TestPlanNamesTasksThatCannotSeparate(t *testing.T) {
	t.Parallel()
	f, _ := experimentFixture(t)
	ctx := context.Background()
	expect(t, f.run(ctx, "experiment", "new", "lean-ab", "--b", "lean", "--task", "value", "--goal", "better", "--repeats", "1", "--seed", "5"), ExitOK)
	none := jsonRun(t, f, ExitOK, "experiment", "plan", "lean-ab")
	if got, ok := none.get("tasks_passed_every_time").([]any); !ok || len(got) != 0 {
		t.Errorf("no graded runs: %s", none.stdout)
	}
	id := taskIDs(t, f)["value"]
	graded := func(passed bool) store.Run {
		return store.Run{Kind: "task", TaskID: id, TaskName: "value", Outcome: agent.OutcomeOK, Passed: &passed, Record: []byte(`{}`)}
	}
	saveRuns(t, f, graded(true), graded(true), graded(true))
	if got := jsonRun(t, f, ExitOK, "experiment", "plan", "lean-ab").get("tasks_passed_every_time").([]any); len(got) != 0 {
		t.Errorf("3 graded runs are too few: %v", got)
	}
	saveRuns(t, f, graded(true))
	easy := jsonRun(t, f, ExitOK, "experiment", "plan", "lean-ab")
	if got := easy.get("tasks_passed_every_time").([]any); len(got) != 1 || got[0] != "value" || len(easy.get("tasks_never_passed").([]any)) != 0 {
		t.Errorf("4 passes: %s", easy.stdout)
	}
	if easy.get("ready") != true {
		t.Errorf("a caution made the plan not ready: %s", easy.stdout)
	}
	*f.terminal = true
	screen := flat(term.Plain(f.run(ctx, "experiment", "plan", "lean-ab").stdout))
	if want := "1 task passed every time so far, so it may not tell the versions apart: value"; !strings.Contains(screen, want) {
		t.Errorf("the screen lacks %q:\n%s", want, screen)
	}
}

// flat is a screen without its frames, its lines joined, so a phrase a panel wrapped is one again.
func flat(screen string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("│", " ", "|", " ").Replace(screen)), " ")
}
