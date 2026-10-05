package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/run"
	"github.com/pigeaca/agentium/internal/term"
)

// Checks are counted over the counted runs of each arm; unread runs are neither met nor unmet; names and patterns are
// redacted; and a report without checks has none of it, in any format.
func TestReportRuleChecks(t *testing.T) {
	in := fixture()
	without, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var plainJSON bytes.Buffer
	if err := without.JSON(&plainJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plainJSON.String(), `"checks"`) {
		t.Error("a report built without checks has the checks key")
	}

	secret := "sk-" + "ant-api03-" + strings.Repeat("abcdefghij", 3) // built here: a key in the source would trip the secret scan
	in.Checks = []run.Check{
		{Name: "ran-tests", Kind: run.CheckRan, Pattern: "go test"},
		{Name: "kept-docs", Kind: run.CheckNotChanged, Pattern: "docs/**"},
		{Name: "leaky", Kind: run.CheckChanged, Pattern: secret},
	}
	in.CheckResults = map[string][]run.CheckResult{}
	armCounted := map[string]int{}
	for i, r := range in.Runs {
		if !counted(r.Record) {
			continue
		}
		armCounted[r.Record.Arm]++
		results := []run.CheckResult{run.CheckMet, run.CheckUnmet, run.CheckUnread}
		if i%2 == 0 {
			results[0], results[1] = run.CheckUnmet, run.CheckMet
		}
		in.CheckResults[r.ID] = results
	}
	// A run that is not counted is not in the counts, whatever it has stored.
	in.CheckResults["not-a-run"] = []run.CheckResult{run.CheckMet, run.CheckMet, run.CheckMet}
	rep, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Checks) != 3 || rep.Checks[0].Name != "ran-tests" {
		t.Fatalf("checks = %+v", rep.Checks)
	}
	for _, a := range rep.Arms {
		ran, kept, leaky := rep.Checks[0].Arms[a.Name], rep.Checks[1].Arms[a.Name], rep.Checks[2].Arms[a.Name]
		if ran.Counted != a.Counted || ran.Counted != armCounted[a.Name] || ran.Met+kept.Met != ran.Counted || ran.Unread != 0 {
			t.Errorf("arm %s: %+v %+v", a.Name, ran, kept)
		}
		if leaky.Unread != a.Counted || leaky.Met != 0 {
			t.Errorf("arm %s: an unread check = %+v", a.Name, leaky)
		}
	}
	if strings.Contains(rep.Checks[2].Pattern, "abcdefghij") {
		t.Errorf("the pattern of a check is not redacted: %q", rep.Checks[2].Pattern)
	}

	var md, js, txt bytes.Buffer
	if err := rep.Markdown(&md); err != nil {
		t.Fatal(err)
	}
	if err := rep.JSON(&js); err != nil {
		t.Fatal(err)
	}
	if err := rep.Terminal(&txt, term.Style{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Rule checks", "not a verdict", "| `ran-tests` | ran a command containing \"go test\" |", "unread)"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("the Markdown lacks %q", want)
		}
	}
	if i, j := strings.Index(md.String(), "## Behavior"), strings.Index(md.String(), "## Rule checks"); i < 0 || j < i || j > strings.Index(md.String(), "## Per task") {
		t.Error("the checks table is not between the behavior and per-task tables")
	}
	if !strings.Contains(txt.String(), "Rule checks") || !strings.Contains(txt.String(), "kept-docs") {
		t.Errorf("the terminal report lacks the checks:\n%s", txt.String())
	}
	var doc struct {
		Checks []struct {
			Name, Kind, Pattern string
			Arms                map[string]struct{ Met, Counted, Unread int }
		}
	}
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil || len(doc.Checks) != 3 || doc.Checks[1].Kind != "not-changed" || len(doc.Checks[1].Arms) != 2 {
		t.Errorf("the JSON checks: %+v, %v", doc.Checks, err)
	}
	if !strings.Contains(js.String(), `"met"`) || !strings.Contains(js.String(), `"unread"`) {
		t.Error("the JSON cells lack met or unread")
	}
}
