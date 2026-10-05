package task

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// Giveaways lists, sorted, the names a text gives away: names the reference solution's non-test files declare, that no
// base file of the same language family contains as a word, that no hidden test file contains as a word, that
// in.Instruction (the task's stored text: what the agent is told today) does not contain as a word, and that text
// contains as a word, all case-sensitively. A task text that names them tells the agent how the reference did it, which
// the hidden tests do not ask for. It is the drafts' second check (Draft).
//
// The declarations are the fairness check's: for Go, package-level names and struct and interface members (newNames);
// for Java, Kotlin, Rust, Python and TypeScript or JavaScript, what declaredNames finds, overrides left out
// (referenceNames). Names under minName characters are skipped, as there. A hidden test's every word counts as a use,
// its strings and comments included: an exact message the tests compare against must be stated (Gaps), so a name in it
// is never a giveaway. The limits are the declarations' (fairness.go); a name equal to any word of the base in its
// family is missed, as is a declaration the patterns do not see.
func (f *Fairness) Giveaways(ctx context.Context, in FairnessInput, text string) ([]string, error) {
	declared := map[string]map[string]bool{} // name -> the extensions of its family
	add := func(name string, exts []string) {
		if declared[name] == nil {
			declared[name] = map[string]bool{}
		}
		for _, e := range exts {
			declared[name][e] = true
		}
	}
	goNames, _, err := f.newNames(ctx, in)
	if err != nil {
		return nil, err
	}
	for name := range goNames {
		add(name, []string{".go"})
	}
	families, err := f.referenceNames(ctx, in)
	if err != nil {
		return nil, err
	}
	for key, names := range families {
		for name := range names {
			add(name, strings.Split(key, ","))
		}
	}
	if len(declared) == 0 {
		return nil, nil
	}
	sol, err := f.source(ctx, in.Solution)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, p := range in.HiddenTests {
		if !source.Has(sol, p) { // a test file the solution removes
			continue
		}
		data, err := sol.ReadFile(p)
		if err != nil {
			continue
		}
		for _, n := range nameToken.FindAllString(string(data), -1) {
			used[n] = true
		}
	}
	var out []string
	for name, exts := range declared {
		if len(name) < minName || used[name] || !caseWordIn(text, name) || caseWordIn(in.Instruction, name) {
			continue
		}
		var specs []string
		for e := range exts {
			specs = append(specs, ":(glob)**/*"+e)
		}
		sort.Strings(specs)
		inBase, err := f.grep(ctx, in.Base, name, true, specs)
		if err != nil {
			return nil, err
		}
		if !inBase {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// caseWordIn reports whether name is in text as a whole word, case-sensitively: the prose word "merge" does not name a
// helper Merge (a sentence that starts with it still does).
func caseWordIn(text, name string) bool {
	return regexp.MustCompile(`(^|[^\w])` + regexp.QuoteMeta(name) + `($|[^\w])`).MatchString(text)
}
