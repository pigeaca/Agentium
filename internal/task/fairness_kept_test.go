package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pigeaca/agentium/internal/gitx/gitxtest"
	"github.com/pigeaca/agentium/internal/store"
)

// gapTasks commits five pairs of a base and a solution in one repository and returns them as tasks: the three unfair
// cases, the same with an instruction that states one of them, a new field named like an old parameter, a Python
// message, and a regression test that needs nothing new. Each base follows the solution before it, as mined tasks do.
func gapTasks(t *testing.T) (repo string, tasks []store.Task) {
	t.Helper()
	repo = t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	add := func(name string, base, solution map[string]string, instruction string) {
		t.Helper()
		b := commit(t, repo, base, name+": base")
		s := commit(t, repo, solution, name+": solution")
		hidden, reference, err := Split(context.Background(), b, s, "-C", repo)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, store.Task{Name: name, BaseCommit: b, SolutionCommit: s, Instruction: instruction, HiddenTests: hidden, Reference: reference})
	}
	add("unfair", fairBase, fairSolution, "Make Include reject non-documents and update the note.")
	add("stated", map[string]string{"ctx/ctx.go": fairBase["ctx/ctx.go"], "ctx/ctx_test.go": fairBase["ctx/ctx_test.go"]}, fairSolution,
		"Add a SlashCommands field to Expect. Note returns \"graded with the starting version\".")
	add("field",
		map[string]string{"p/p.go": "package p\n\ntype Config struct{}\n\nfunc Wait(timeout int) {}\n"},
		map[string]string{
			"p/p.go":      "package p\n\ntype Config struct{ Timeout int }\n\nfunc Wait(timeout int) {}\n",
			"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) { _ = Config{Timeout: 5} }\n",
		}, "")
	add("python",
		map[string]string{"app/svc.py": "def run():\n    return 1\n"},
		map[string]string{
			"app/svc.py":            "def run():\n    raise ValueError('quota exceeded for this account')\n",
			"tests/test_service.py": "def test_run():\n    assert 'quota exceeded for this account' in str(run())\n",
		}, "Make run fail.")
	add("regression",
		map[string]string{"q/q.go": "package q\n\nfunc Double(n int) int { return n + n }\n"},
		map[string]string{
			"q/q.go":      "package q\n\nfunc Double(n int) int { return 2 * n }\n",
			"q/q_test.go": "package q\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
		}, "Fix Double.")
	return repo, tasks
}

// gapsAlone works out each task's gaps with a checker of its own, as a command that shows one task does.
func gapsAlone(t *testing.T, repo string, tasks []store.Task) [][]Gap {
	t.Helper()
	var want [][]Gap
	for _, tk := range tasks {
		gaps, err := Gaps(context.Background(), NewFairness("-C", repo), tk)
		if err != nil {
			t.Fatalf("%s: %v", tk.Name, err)
		}
		want = append(want, gaps)
	}
	return want
}

// A checker keeps what it worked out and what it read: the same task asked again starts no git process, and a task
// that differs only in its instruction repeats no git call (its searches and files are the kept ones).
func TestFairnessKeepsWhatItWorkedOut(t *testing.T) {
	repo, tasks := gapTasks(t)
	ctx := context.Background()
	want := gapsAlone(t, repo, tasks)
	if texts := gapTexts(want[0]); !slices.Equal(texts, []string{"identifier:SlashCommands", "literal:graded with the starting version", "literal:" + errText}) {
		t.Fatalf("the fixture's first task: %q", texts)
	}
	calls := gitxtest.Calls(t)
	f := NewFairness("-C", repo)
	first, err := Gaps(ctx, f, tasks[0])
	if err != nil || !reflect.DeepEqual(first, want[0]) {
		t.Fatalf("gaps = %v, %v; want %v", first, err, want[0])
	}
	made := calls()
	if len(made) == 0 || len(gitxtest.Repeated(made)) != 0 {
		t.Errorf("the first check repeated git calls: %v", gitxtest.Repeated(made))
	}
	first[0].Text = "changed by the caller" // the caller's own copy
	again, err := Gaps(ctx, f, tasks[0])
	if err != nil || !reflect.DeepEqual(again, want[0]) {
		t.Errorf("asked again: %v, %v; want %v", again, err, want[0])
	}
	if more := calls(); len(more) != 0 {
		t.Errorf("asked again: git calls %v, want none", more)
	}
	// Another instruction is another input, checked afresh, from the same files and searches.
	stated := tasks[0]
	stated.Instruction = "Add a SlashCommands field to Expect."
	gaps, err := Gaps(ctx, f, stated)
	if err != nil || !slices.Equal(gapTexts(gaps), []string{"literal:graded with the starting version", "literal:" + errText}) {
		t.Errorf("with the field stated: %q, %v", gapTexts(gaps), err)
	}
	if repeated := gitxtest.Repeated(append(made, calls()...)); len(repeated) != 0 {
		t.Errorf("another instruction repeated git calls: %v", repeated)
	}
}

// PrepareGaps checks several tasks at a time; what it keeps is what each task's own check gives, so the calls that
// follow answer the same, in any order, without git. A task that cannot be checked keeps nothing and fails as before.
func TestPrepareGapsGivesEachTaskItsOwnGaps(t *testing.T) {
	repo, tasks := gapTasks(t)
	ctx := context.Background()
	want := gapsAlone(t, repo, tasks)
	broken := tasks[0]
	broken.Name, broken.SolutionCommit = "broken", strings.Repeat("0", 40)
	_, wantErr := Gaps(ctx, NewFairness("-C", repo), broken)
	if wantErr == nil {
		t.Fatal("a task whose solution the repository lacks was checked")
	}
	unsolved := store.Task{Name: "unsolved", BaseCommit: tasks[0].BaseCommit, Instruction: "No solution: nothing to check."}

	f := NewFairness("-C", repo)
	all := append(slices.Clone(tasks), broken, unsolved)
	all = append(all, tasks...) // the same task twice in one list is checked once
	calls := gitxtest.Calls(t)
	PrepareGaps(ctx, f, all)
	if repeated := gitxtest.Repeated(calls()); len(repeated) != 0 {
		t.Errorf("git asked twice for: %v", repeated)
	}
	for _, i := range []int{3, 0, 4, 1, 2, 0} {
		got, err := Gaps(ctx, f, tasks[i])
		if err != nil || !reflect.DeepEqual(got, want[i]) {
			t.Errorf("%s: %v, %v; want %v", tasks[i].Name, got, err, want[i])
		}
	}
	if gaps, err := Gaps(ctx, f, unsolved); err != nil || gaps != nil {
		t.Errorf("a task without a solution: %v, %v", gaps, err)
	}
	if made := calls(); len(made) != 0 {
		t.Errorf("prepared tasks asked git again: %v", made)
	}
	if _, err := Gaps(ctx, f, broken); err == nil || err.Error() != wantErr.Error() {
		t.Errorf("the broken task: %v, want %v", err, wantErr)
	}
	PrepareGaps(ctx, f, nil) // nothing to do, and nothing to wait for
}

// A cancelled preparation ends, keeps nothing, and leaves the checker able to check. A command interrupted later is
// not given what it kept before: its check runs, and fails at its first read, as it did when nothing was kept.
func TestPrepareGapsCancelled(t *testing.T) {
	repo, tasks := gapTasks(t)
	want := gapsAlone(t, repo, tasks)
	f := NewFairness("-C", repo)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	PrepareGaps(cancelled, f, tasks)
	if _, err := Gaps(cancelled, f, tasks[0]); err == nil {
		t.Error("a cancelled check gave gaps")
	}
	calls := gitxtest.Calls(t)
	ctx, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	PrepareGaps(ctx, f, tasks)
	if len(calls()) == 0 {
		t.Error("the cancelled preparation kept gaps: the one that followed asked git nothing")
	}
	for i, tk := range tasks {
		if got, err := Gaps(ctx, f, tk); err != nil || !reflect.DeepEqual(got, want[i]) {
			t.Errorf("%s after the cancelled preparation: %v, %v; want %v", tk.Name, got, err, want[i])
		}
	}
	// Kept gaps are given to no context that has ended: not to another one than the check's (whose sources and
	// searches would still answer), and not to the check's own once it is interrupted.
	for _, tk := range tasks {
		if gaps, err := Gaps(cancelled, f, tk); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: a cancelled context was given %v, %v", tk.Name, gaps, err)
		}
	}
	interrupt()
	for _, tk := range tasks {
		if _, err := Gaps(ctx, f, tk); err == nil {
			t.Errorf("%s: an interrupted command was given kept gaps", tk.Name)
		}
	}
}

// A check skips a file it cannot read, so an answer worked out while a read failed may lack something: it is given,
// as it always was, but not kept, and the next check reads the file again. The same goes for the base's fields.
func TestFairnessKeepsNoGapsWorkedOutFromAFailedRead(t *testing.T) {
	repo, tasks := gapTasks(t)
	want := gapsAlone(t, repo, tasks)
	field := tasks[2] // its one gap, the field Timeout, is known from the reference file p/p.go alone
	if texts := gapTexts(want[2]); !slices.Equal(texts, []string{"identifier:Timeout"}) {
		t.Fatalf("the fixture's field task: %q", texts)
	}
	ctx := context.Background()
	away := func(spec string) (back func()) {
		t.Helper()
		blob := git(t, repo, "rev-parse", spec)
		object := filepath.Join(repo, ".git", "objects", blob[:2], blob[2:])
		if err := os.Rename(object, object+".away"); err != nil {
			t.Fatal(err)
		}
		return func() {
			t.Helper()
			if err := os.Rename(object+".away", object); err != nil {
				t.Fatal(err)
			}
		}
	}

	f := NewFairness("-C", repo)
	back := away(field.SolutionCommit + ":p/p.go")
	if gaps, err := Gaps(ctx, f, field); err != nil || len(gaps) != 0 {
		t.Fatalf("with the reference file unreadable: %v, %v; the check skips it and finds nothing", gaps, err)
	}
	back()
	if gaps, err := Gaps(ctx, f, field); err != nil || !reflect.DeepEqual(gaps, want[2]) {
		t.Errorf("once the file can be read: %v, %v; want %v (the incomplete answer was kept)", gaps, err, want[2])
	}
	calls := gitxtest.Calls(t)
	if gaps, err := Gaps(ctx, f, field); err != nil || !reflect.DeepEqual(gaps, want[2]) || len(calls()) != 0 {
		t.Errorf("the complete answer is kept: %v, %v", gaps, err)
	}

	f = NewFairness("-C", repo)
	back = away(field.BaseCommit + ":p/p.go")
	if fields, err := f.baseFields(ctx, field.BaseCommit, []string{"p"}); err != nil || len(fields) != 0 {
		t.Fatalf("the base's fields with its file unreadable: %v, %v", fields, err)
	}
	back()
	fields, err := f.baseFields(ctx, field.BaseCommit, []string{"p"})
	if _, ok := fields["Config"]; err != nil || !ok {
		t.Errorf("the base's fields once the file can be read: %v, %v; want Config's (the incomplete ones were kept)", fields, err)
	}
}

// No two inputs share a key, wherever their texts are cut and whatever bytes they hold.
func TestFairnessInputKeysTellInputsApart(t *testing.T) {
	inputs := []FairnessInput{
		{},
		{Base: "a"},
		{Solution: "a"},
		{Instruction: "a"},
		{HiddenTests: []string{"a"}},
		{Reference: []string{"a"}},
		{Base: "ab", Solution: "c"},
		{Base: "a", Solution: "bc"},
		{HiddenTests: []string{"a", "b"}},
		{HiddenTests: []string{"a"}, Reference: []string{"b"}},
		{HiddenTests: []string{"ab"}},
		{HiddenTests: []string{"a\x00b"}},
		{HiddenTests: []string{""}},
		{Reference: []string{""}},
		{Instruction: "1\x00a"},
		{Instruction: "\xff"},
		{Instruction: "\xfe"},
		{Base: "1", Solution: "1", Instruction: "0\x000\x00"},
	}
	seen := map[string]int{}
	for i, in := range inputs {
		if j, ok := seen[in.key()]; ok {
			t.Errorf("inputs %d and %d share the key %q: %+v, %+v", j, i, in.key(), inputs[j], in)
		}
		seen[in.key()] = i
	}
	if a, b := (FairnessInput{Base: "b", HiddenTests: []string{"x"}}), (FairnessInput{Base: "b", HiddenTests: []string{"x"}}); a.key() != b.key() {
		t.Error("equal inputs have different keys")
	}
}

// kept works a key's value out once, however many goroutines ask at the same time; a failure is not kept, nor is an
// answer whose work says it must not be, and each caller that meets one gets the result of its own attempt.
func TestKeptWorksEachKeyOutOnce(t *testing.T) {
	ctx := context.Background()
	var k kept[int]
	var worked atomic.Int32
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := []string{"a", "b"}[i%2]
			v, err := k.get(ctx, key, func() (int, bool, error) {
				worked.Add(1)
				return len(key) + i%2, true, nil
			})
			if err != nil || v != 1+i%2 {
				t.Errorf("%s: %d, %v", key, v, err)
			}
		}()
	}
	wg.Wait()
	if n := worked.Load(); n != 2 {
		t.Errorf("worked %d values out for 2 keys", n)
	}
	fail := errors.New("no")
	for attempt := range 2 {
		if _, err := k.get(ctx, "c", func() (int, bool, error) { return 0, true, fail }); !errors.Is(err, fail) {
			t.Errorf("attempt %d: %v, want the failure", attempt, err)
		}
	}
	for attempt := range 2 { // an answer that is not to be kept is given, and worked out again the next time
		if v, err := k.get(ctx, "c", func() (int, bool, error) { return 5 + attempt, false, nil }); err != nil || v != 5+attempt {
			t.Errorf("attempt %d, not to be kept: %d, %v", attempt, v, err)
		}
	}
	if v, err := k.get(ctx, "c", func() (int, bool, error) { return 7, true, nil }); err != nil || v != 7 {
		t.Errorf("after the failures: %d, %v", v, err)
	}
	if v, err := k.get(ctx, "c", func() (int, bool, error) { return 0, true, fail }); err != nil || v != 7 {
		t.Errorf("kept value: %d, %v", v, err)
	}
}

// Who waits for another goroutine's work waits only as long as its own context lives; when that work fails, is not
// to be kept, or panics, those who waited work the value out themselves.
func TestKeptWaitsNoLongerThanTheCallersContext(t *testing.T) {
	var k kept[string]
	started, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := k.get(context.Background(), "key", func() (string, bool, error) {
			close(started)
			<-release
			return "not to be kept", false, nil
		})
		first <- err
	}()
	<-started
	waiting, stop := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() {
		_, err := k.get(waiting, "key", func() (string, bool, error) { return "", true, errors.New("the waiter worked while another did") })
		waited <- err
	}()
	stop()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Errorf("a waiter whose context ended: %v, want its context's error", err)
	}
	// A second waiter stays, and works the value out itself once the first attempt ends with nothing kept.
	second := make(chan string, 1)
	go func() {
		v, _ := k.get(context.Background(), "key", func() (string, bool, error) { return "the waiter's own", true, nil })
		second <- v
	}()
	close(release)
	if err := <-first; err != nil {
		t.Errorf("the first attempt: %v", err)
	}
	if v := <-second; v != "the waiter's own" {
		t.Errorf("the waiter got %q, want its own value: the first was not to be kept", v)
	}

	// A panic in the work frees the key: the next caller is not left waiting.
	func() {
		defer func() { _ = recover() }()
		_, _ = k.get(context.Background(), "panics", func() (string, bool, error) { panic("boom") })
	}()
	if v, err := k.get(context.Background(), "panics", func() (string, bool, error) { return "after", true, nil }); err != nil || v != "after" {
		t.Errorf("after a panic: %q, %v", v, err)
	}
}

// overtaken is a context that a cancel overtakes: it reports no error the first time it is asked, and
// context.Canceled from then on, while nothing it started is stopped (its git calls all finish).
type overtaken struct {
	context.Context
	asked atomic.Bool
}

func (c *overtaken) Err() error {
	if c.asked.Swap(true) {
		return context.Canceled
	}
	return nil
}

// A check that ends after its context was cancelled is an error, and keeps nothing: a read that a cancel skipped
// would make its answer smaller than it is. The checks here need no search, so only the cancel can fail them.
func TestFairnessCheckOvertakenByACancelKeepsNothing(t *testing.T) {
	repo, tasks := gapTasks(t)
	want := gapsAlone(t, repo, tasks)
	live := context.Background()
	for _, i := range []int{2, 4} { // "field" reads the base's struct fields; "regression" reads its two files alone
		f := NewFairness("-C", repo)
		if gaps, err := Gaps(&overtaken{Context: live}, f, tasks[i]); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v, %v; want the cancel's error", tasks[i].Name, gaps, err)
		}
		if got, err := Gaps(live, f, tasks[i]); err != nil || !reflect.DeepEqual(got, want[i]) {
			t.Errorf("%s after the overtaken check: %v, %v; want %v", tasks[i].Name, got, err, want[i])
		}
	}
	f := NewFairness("-C", repo)
	field := tasks[2]
	late := &overtaken{Context: live}
	late.asked.Store(true)
	if fields, err := f.baseFields(late, field.BaseCommit, []string{"p"}); !errors.Is(err, context.Canceled) {
		t.Errorf("the base's fields under a cancelled context: %v, %v", fields, err)
	}
	fields, err := f.baseFields(live, field.BaseCommit, []string{"p"})
	if _, ok := fields["Config"]; err != nil || !ok {
		t.Errorf("the base's fields after that: %v, %v; want Config's", fields, err)
	}
}
