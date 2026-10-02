package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/experiment"
	"github.com/pigeaca/agentium/internal/stats"
	"github.com/pigeaca/agentium/internal/term"
)

// modelABFixture is the context A/B fixture's runs read as a model A/B on one context: arm A on Opus at high effort,
// arm B (about 20% cheaper) on Sonnet.
func modelABFixture() Input {
	in := fixture()
	l := &in.Lock
	l.Design.Template, l.Design.Version = experiment.TemplateModelAB, experiment.Design{Template: experiment.TemplateModelAB}.WantVersion()
	profiles := []struct{ model, effort string }{{"claude-opus-5-5", "high"}, {"claude-sonnet-5-5", ""}}
	for i := range l.Design.Arms {
		a := &l.Design.Arms[i]
		a.Context, a.Snapshot, a.Model, a.Effort = "base", "", profiles[i].model, profiles[i].effort
		l.Arms[i].Arm = *a
		l.Arms[i].Snapshot = ""
	}
	return in
}

// Every place that named an arm by "A" or "B" names its profile in a model-ab report, in all three formats.
func TestModelABReportsNameArmsByProfile(t *testing.T) {
	t.Parallel()
	rep, err := Build(modelABFixture())
	if err != nil {
		t.Fatal(err)
	}
	const a, b = "A (claude-opus-5-5:high)", "B (claude-sonnet-5-5)"
	if !strings.HasPrefix(rep.Summary, "B (claude-sonnet-5-5) costs ") || !strings.Contains(rep.Summary, "% less; success: ") {
		t.Errorf("summary %q, want \"B (claude-sonnet-5-5) costs N%% less; success: ...\"", rep.Summary)
	}
	var terminal, markdown, encoded bytes.Buffer
	if err := rep.Terminal(&terminal, term.Style{}); err != nil {
		t.Fatal(err)
	}
	if err := rep.Markdown(&markdown); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&encoded); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"terminal": terminal.String(), "markdown": markdown.String()} {
		for _, want := range []string{
			rep.Summary,                 // the verdict sentence
			"Cost of " + b + " vs " + a, // a headline
			"Success: " + a + " ",       // the other headline
			a + " ", b + " ",            // metric, cost, behavior and per-task headings
			"(A: claude-opus-5-5:high) and ", "(B: claude-sonnet-5-5);", // the pass line
			"Measured on " + a + " and " + b + " together", // the noise note
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s report lacks %q:\n%s", name, want, got)
			}
		}
	}
	// Per-task rows: the heading of each arm's column carries the profile.
	if !strings.Contains(markdown.String(), "| Task | "+a+" | "+b+" | Cost A → B |") {
		t.Errorf("per-task heading:\n%s", markdown.String())
	}
	var parsed struct {
		Summary string `json:"summary"`
		Arms    []struct {
			Name, Profile string
		} `json:"arms"`
		Tasks []struct {
			Arms map[string]struct{ Profile string } `json:"arms"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(encoded.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Summary != rep.Summary || parsed.Arms[0].Profile != "claude-opus-5-5:high" || parsed.Arms[1].Profile != "claude-sonnet-5-5" ||
		parsed.Tasks[0].Arms["A"].Profile != "claude-opus-5-5:high" || parsed.Tasks[0].Arms["B"].Profile != "claude-sonnet-5-5" {
		t.Errorf("JSON profiles: %+v", parsed)
	}
}

// A context experiment's report names no profiles: its golden files hold, and these fields stay out of its JSON.
func TestContextReportsNameNoProfiles(t *testing.T) {
	t.Parallel()
	rep, err := Build(fixture())
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := rep.JSON(&encoded); err != nil {
		t.Fatal(err)
	}
	if rep.Summary != "" || strings.Contains(encoded.String(), `"profile"`) || strings.Contains(encoded.String(), `"summary"`) {
		t.Errorf("a context report names profiles: summary %q", rep.Summary)
	}
}

func TestSummaryPartReadsDirectionAndSize(t *testing.T) {
	t.Parallel()
	cost := func(verdict string, estimate float64) experiment.MetricResult {
		res := experiment.MetricResult{Metric: experiment.MetricCost, Ratio: true, Tasks: 8, Verdict: verdict}
		res.Boot95.Estimate, res.T95.Estimate = estimate, estimate
		res.Boot95.Low, res.T95.Low, res.Boot95.High, res.T95.High = estimate-0.1, estimate-0.1, estimate+0.1, estimate+0.1
		return res
	}
	for _, c := range []struct {
		res    experiment.MetricResult
		want   string
		phrase bool
	}{
		{cost("improved", 0.52), "costs 48% less", true},
		{cost("regressed", 2.3), "costs 2.3× more", true},
		{cost("regressed", 1.4), "costs 40% more", true},
		{cost("exploratory", 0.9), "cost: exploratory", false},
		{experiment.MetricResult{Metric: experiment.MetricSuccess, Tasks: 8, Verdict: "inconclusive"}, "success: inconclusive", false},
		{experiment.MetricResult{Metric: experiment.MetricCost, Ratio: true, Tasks: 1}, "cost: no result", false},
	} {
		if got, phrase := summaryPart(c.res); got != c.want || phrase != c.phrase {
			t.Errorf("summaryPart(%s %s) = %q, %v; want %q, %v", c.res.Metric, c.res.Verdict, got, phrase, c.want, c.phrase)
		}
	}
}

// "Improved (small)" and "no loss" favour arm B as "improved" does: runs cut short in B put the caveat in the cost
// headline, and a short one in the model-ab summary; in arm A they do not.
func TestCaveatOnEveryCostVerdictFavouringB(t *testing.T) {
	t.Parallel()
	for _, verdict := range []string{stats.ImprovedSmall, stats.NoLoss} {
		res := experiment.MetricResult{Metric: experiment.MetricCost, Role: experiment.RolePrimary, Ratio: true, Tasks: 8, Verdict: verdict}
		res.Boot95.Estimate, res.T95.Estimate = 0.95, 0.95
		res.Boot95.Low, res.T95.Low, res.Boot95.High, res.T95.High = 0.92, 0.92, 0.98, 0.98
		for cutShort, want := range map[string]bool{"B": true, "A": false} {
			rep := Report{Arms: []Arm{{Name: "A", Profile: "claude-opus-5-5"}, {Name: "B", Profile: "claude-sonnet-5-5"}},
				Analysis: experiment.Analysis{Results: []experiment.MetricResult{res}}}
			rep.Lock.Design = experiment.Design{Template: experiment.TemplateModelAB, CostMargin: 0.1, SuccessMargin: 0.15}
			rep.Arms[map[string]int{"A": 0, "B": 1}[cutShort]].Capped = 1
			_, _, headline := rep.headlineParts(res)
			summary := summarize(rep)
			if strings.Contains(headline, "caveat: runs cut short at their cap or the timeout, 1 in arm B") != want ||
				strings.Contains(summary, "(caveat: runs cut short favour it)") != want {
				t.Errorf("%s, cut short in %s: headline %q, summary %q; caveat wanted %v", verdict, cutShort, headline, summary, want)
			}
		}
	}
}
