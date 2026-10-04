package run

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/judge"
)

// spendFields says how Spend accounts for every money field a run record holds (a float64 named *USD, at any depth):
//   - "agent", "judge" and "pair": the field is Spend's AgentUSD, JudgeUSD or PairJudgeUSD, and so in TotalUSD;
//   - "folded": counted elsewhere, or not spend: Metrics.EstimatedCostUSD is an estimate that Once and recovery copy
//     into Metrics.CostUSD when Claude Code reported no cost, so it is counted there, never on its own;
//     IsolatedCostUSD is a counterfactual (the run's cost without other runs' prompt cache) that nobody paid;
//     Overshoot's fields describe Metrics.CostUSD against the run's cap (its cap, how far past it, the allowance held),
//     so the cost is already counted there; a pair comparison's orders (PairJudge.Verdict.AB and BA) are parts of its
//     Verdict.CostUSD.
//
// A new money field fails TestSpendCoversEveryCost until it is added here and to Spend.
var spendFields = map[string]string{
	"Metrics.CostUSD":              "agent",
	"Metrics.EstimatedCostUSD":     "folded",
	"IsolatedCostUSD":              "folded",
	"Judge.CostUSD":                "judge",
	"PairJudge.Verdict.CostUSD":    "pair",
	"PairJudge.Verdict.AB.CostUSD": "folded",
	"PairJudge.Verdict.BA.CostUSD": "folded",
	"Overshoot.CapUSD":             "folded",
	"Overshoot.OverUSD":            "folded",
	"Overshoot.AllowanceUSD":       "folded",
	"CapUSD":                       "folded", // a Codex run's cap: a limit, counted as spend only through Metrics.CostUSD (codexSpendFallback)
}

// moneyFields lists the paths of the float64 fields (or pointers to one) named *USD under t, through structs,
// pointers, slices and maps.
func moneyFields(t reflect.Type, prefix string, seen map[reflect.Type]bool, out *[]string) {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		moneyFields(t.Elem(), prefix, seen, out)
		return
	case reflect.Struct:
	default:
		return
	}
	if seen[t] {
		return
	}
	seen[t] = true
	defer delete(seen, t)
	for i := range t.NumField() {
		f := t.Field(i)
		path := prefix + f.Name
		leaf := f.Type
		if leaf.Kind() == reflect.Pointer {
			leaf = leaf.Elem()
		}
		if leaf.Kind() == reflect.Float64 && strings.HasSuffix(f.Name, "USD") {
			*out = append(*out, path)
			continue
		}
		moneyFields(f.Type, path+".", seen, out)
	}
}

// setField sets the float64 at path in rec, allocating the pointers on the way.
func setField(t *testing.T, rec *Record, path string, value float64) {
	t.Helper()
	v := reflect.ValueOf(rec).Elem()
	for _, name := range strings.Split(path, ".") {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			t.Fatalf("%s: cannot reach %s (a %s on the way)", path, name, v.Kind())
		}
		v = v.FieldByName(name)
	}
	if v.Kind() == reflect.Pointer { // a pointer to a float64 is a leaf
		v.Set(reflect.New(v.Type().Elem()))
		v = v.Elem()
	}
	v.SetFloat(value)
}

func TestSpendCoversEveryCost(t *testing.T) {
	var found []string
	moneyFields(reflect.TypeFor[Record](), "", map[reflect.Type]bool{}, &found)
	for _, path := range found {
		if _, ok := spendFields[path]; !ok {
			t.Errorf("Record.%s holds money that Spend does not account for: add it to Spend (Record.Spend, StoredSpend, TotalUSD) and to spendFields", path)
		}
	}
	for path := range spendFields {
		if !slices.Contains(found, path) {
			t.Errorf("spendFields names %s, which Record no longer has", path)
		}
	}

	for _, path := range found {
		kind := spendFields[path]
		if kind == "" {
			continue
		}
		var rec Record
		setField(t, &rec, path, 1.25)
		got := rec.Spend()
		want := Spend{}
		switch kind {
		case "agent":
			want.AgentUSD = 1.25
		case "judge":
			want.JudgeUSD = 1.25
		case "pair":
			want.PairJudgeUSD = 1.25
		}
		if got != want {
			t.Errorf("%s at $1.25: Spend = %+v, want %+v", path, got, want)
		}
		if kind != "folded" && got.TotalUSD() != 1.25 {
			t.Errorf("%s at $1.25: TotalUSD = %v, want it counted", path, got.TotalUSD())
		}
		// A stored run's spend is the same: its cost column holds the agent's, the rest comes from its record.
		encoded, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if stored := StoredSpend(rec.Metrics.CostUSD, encoded); stored != got {
			t.Errorf("%s at $1.25: StoredSpend = %+v, want the record's %+v", path, stored, got)
		}
	}
}

// TestSpendTotalsEveryPart fails when Spend gains a money field that TotalUSD leaves out.
func TestSpendTotalsEveryPart(t *testing.T) {
	st := reflect.TypeFor[Spend]()
	for i := range st.NumField() {
		f := st.Field(i)
		if f.Type.Kind() != reflect.Float64 {
			continue
		}
		var s Spend
		reflect.ValueOf(&s).Elem().Field(i).SetFloat(0.75)
		if s.TotalUSD() != 0.75 {
			t.Errorf("Spend.%s at $0.75: TotalUSD = %v, want it counted", f.Name, s.TotalUSD())
		}
	}
}

func TestSpend(t *testing.T) {
	rec := Record{CostEstimated: true, Judge: &judge.Verdict{CostUSD: 0.1}}
	rec.Metrics.CostUSD, rec.Metrics.EstimatedCostUSD = 0.3, 0.3 // an estimate copied into the cost counts once
	s := rec.Spend()
	if s != (Spend{AgentUSD: 0.3, JudgeUSD: 0.1, AgentEstimated: true}) || s.TotalUSD() != 0.3+0.1 || rec.JudgeCostUSD() != 0.1 {
		t.Errorf("spend = %+v, total %v", s, s.TotalUSD())
	}
	if (Record{}).Spend() != (Spend{}) {
		t.Errorf("a run without costs spent %+v", Record{}.Spend())
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if stored := StoredSpend(0.3, encoded); stored != s {
		t.Errorf("stored spend = %+v, want %+v", stored, s)
	}
	// An unreadable record still counts its agent's cost from the column, as before.
	if stored := StoredSpend(0.2, []byte("{not json")); stored != (Spend{AgentUSD: 0.2}) {
		t.Errorf("unreadable record: %+v", stored)
	}
}
