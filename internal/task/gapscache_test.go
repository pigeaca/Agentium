package task

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/gitx/gitxtest"
	"github.com/pigeaca/agentium/internal/store"
)

// listed checks tasks as a list does with a kept file: a checker that takes from the file at path and puts there what
// it works out, several tasks at a time, saved at the end. It returns each task's gaps.
func listed(t *testing.T, repo, path, build, git string, tasks []store.Task) [][]Gap {
	t.Helper()
	ctx := context.Background()
	kept := OpenGapsCache(path, build, git)
	f := NewFairness("-C", repo)
	f.Keep(kept)
	PrepareGaps(ctx, f, tasks)
	var out [][]Gap
	for _, tk := range tasks {
		gaps, err := Gaps(ctx, f, tk)
		if err != nil {
			t.Fatalf("%s: %v", tk.Name, err)
		}
		out = append(out, gaps)
	}
	if err := kept.Save(tasks); err != nil {
		t.Fatal(err)
	}
	return out
}

// What one command worked out, the next takes from the file: the same gaps, with no git process, and it leaves the
// file as it found it. The file is the owner's alone.
func TestGapsCacheGivesTheNextCommandWhatACheckFound(t *testing.T) {
	repo, tasks := gapTasks(t)
	want := gapsAlone(t, repo, tasks)
	path := filepath.Join(t.TempDir(), "cache", "gaps", "1.json")
	if got := listed(t, repo, path, "build", "git", tasks); !reflect.DeepEqual(got, want) {
		t.Fatalf("the first list: %v, want %v", got, want)
	}
	for _, p := range []string{path, filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if wantMode := map[bool]os.FileMode{true: 0o700, false: 0o600}[info.IsDir()]; info.Mode().Perm() != wantMode {
			t.Errorf("%s has mode %04o, want %04o", p, info.Mode().Perm(), wantMode)
		}
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file gapsFile
	if err := json.Unmarshal(saved, &file); err != nil || file.Format != gapsFormat || len(file.Checkers) != 1 || len(file.Checkers[0].Gaps) != len(tasks) {
		t.Fatalf("the file: %v, format %d, %d checkers; want one with %d tasks' answers:\n%s", err, file.Format, len(file.Checkers), len(tasks), saved)
	}

	calls := gitxtest.Calls(t)
	if got := listed(t, repo, path, "build", "git", tasks); !reflect.DeepEqual(got, want) {
		t.Errorf("the next list: %v, want %v", got, want)
	}
	if made := calls(); len(made) != 0 {
		t.Errorf("the next list asked git: %v", made)
	}
	if again, err := os.ReadFile(path); err != nil || !bytes.Equal(again, saved) {
		t.Errorf("a list that found everything kept rewrote the file (%v)", err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(left) != 0 {
		t.Errorf("files left beside the cache: %v", left)
	}
	// A nil cache keeps nothing and breaks nothing.
	var none *GapsCache
	if _, ok := none.lookup(FairnessInput{}); ok || none.Save(tasks) != nil {
		t.Error("a nil cache answered or failed")
	}
	none.store(FairnessInput{}, nil)
}

// The file keeps each checker's answers apart: another build of Agentium, another git or another locale takes nothing
// of the first's, checks everything itself and adds its own, and neither loses the other's. It holds the checkers
// that saved last, up to maxCheckers.
func TestGapsCacheKeepsEachCheckersAnswersApart(t *testing.T) {
	repo, tasks := gapTasks(t)
	want := gapsAlone(t, repo, tasks)
	path := filepath.Join(t.TempDir(), "gaps.json")
	const git = "git 2.49\nLANG=en_US.UTF-8"
	listed(t, repo, path, "build 1", git, tasks)
	in, _ := inputOf(tasks[0])
	for name, checker := range map[string][2]string{
		"another build":  {"build 2", git},
		"another git":    {"build 1", "git 2.50\nLANG=en_US.UTF-8"},
		"another locale": {"build 1", "git 2.49\nLANG=tr_TR.UTF-8"},
		"shifted texts":  {"build 1git 2.49", "\nLANG=en_US.UTF-8"},
	} {
		if gaps, ok := OpenGapsCache(path, checker[0], checker[1]).lookup(in); ok {
			t.Errorf("%s was given %v", name, gaps)
		}
	}
	calls := gitxtest.Calls(t)
	if got := listed(t, repo, path, "build 2", git, tasks); !reflect.DeepEqual(got, want) {
		t.Errorf("another build's list: %v, want %v", got, want)
	}
	if len(calls()) == 0 {
		t.Error("another build checked nothing itself")
	}
	for _, build := range []string{"build 1", "build 2"} {
		if got := listed(t, repo, path, build, git, tasks); !reflect.DeepEqual(got, want) || len(calls()) != 0 {
			t.Errorf("%s after both saved: it must find all its answers in the file", build)
		}
	}
	// What one checker found is given to no other: an answer only build 3 has, for an input the others have too.
	only := OpenGapsCache(path, "build 3", git)
	only.store(in, []Gap{{Kind: GapLiteral, Text: "only build 3 found this", File: "a_test.go"}})
	if err := only.Save(tasks); err != nil {
		t.Fatal(err)
	}
	if gaps, ok := OpenGapsCache(path, "build 3", git).lookup(in); !ok || len(gaps) != 1 || gaps[0].Text != "only build 3 found this" {
		t.Errorf("build 3's own answer: %v, %v", gaps, ok)
	}
	if gaps, ok := OpenGapsCache(path, "build 1", git).lookup(in); !ok || !reflect.DeepEqual(gaps, want[0]) {
		t.Errorf("build 1's answer after build 3 saved another: %v, %v; want its own %v", gaps, ok, want[0])
	}
	// The checkers that saved last are held; the one that saved longest ago goes when there are more than maxCheckers.
	for i := range maxCheckers - 1 {
		next := OpenGapsCache(path, "later build "+string(rune('a'+i)), git)
		next.store(in, nil)
		if err := next.Save(tasks); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file gapsFile
	if err := json.Unmarshal(data, &file); err != nil || len(file.Checkers) != maxCheckers {
		t.Fatalf("the file holds %d checkers (%v), want %d", len(file.Checkers), err, maxCheckers)
	}
	if _, ok := OpenGapsCache(path, "build 1", git).lookup(in); ok {
		t.Error("the checker that saved longest ago is still in a full file")
	}
	if _, ok := OpenGapsCache(path, "build 3", git).lookup(in); !ok {
		t.Error("a checker among the last to save is gone")
	}
}

// An answer is kept under everything the task's check reads of the task: change any of it and the task is checked
// afresh, while the other tasks' answers are still taken from the file.
func TestGapsCacheChecksAChangedTaskAfresh(t *testing.T) {
	repo, tasks := gapTasks(t)
	path := filepath.Join(t.TempDir(), "gaps.json")
	listed(t, repo, path, "build", "git", tasks)
	kept := OpenGapsCache(path, "build", "git")
	unfair, _ := inputOf(tasks[0])
	if _, ok := kept.lookup(unfair); !ok {
		t.Fatal("the task's answer is not in the file")
	}
	for name, change := range map[string]func(*FairnessInput){
		"the instruction":  func(in *FairnessInput) { in.Instruction += " Add a SlashCommands field to Expect." },
		"the base":         func(in *FairnessInput) { in.Base = tasks[1].BaseCommit },
		"the solution":     func(in *FairnessInput) { in.Solution = tasks[1].SolutionCommit },
		"the hidden tests": func(in *FairnessInput) { in.HiddenTests = append([]string{"other_test.go"}, in.HiddenTests...) },
		"the reference":    func(in *FairnessInput) { in.Reference = nil },
		"a file moved between the lists": func(in *FairnessInput) {
			in.Reference, in.HiddenTests = append(in.Reference, in.HiddenTests[0]), in.HiddenTests[1:]
		},
	} {
		changed := unfair
		changed.HiddenTests, changed.Reference = append([]string{}, unfair.HiddenTests...), append([]string{}, unfair.Reference...)
		change(&changed)
		if gaps, ok := kept.lookup(changed); ok {
			t.Errorf("with %s changed, the old answer was given: %v", name, gaps)
		}
	}
	// A list after an edit: the edited task is checked (git is asked), and it alone.
	edited := append([]store.Task{}, tasks...)
	edited[0].Instruction += " Add a SlashCommands field to Expect."
	calls := gitxtest.Calls(t)
	got := listed(t, repo, path, "build", "git", edited)
	if texts := gapTexts(got[0]); !reflect.DeepEqual(texts, []string{"literal:graded with the starting version", "literal:" + errText}) {
		t.Errorf("the edited task's gaps: %q", texts)
	}
	made := calls()
	if len(made) == 0 {
		t.Fatal("the edited task was not checked")
	}
	for _, call := range made {
		line := strings.Join(call, " ")
		if !strings.Contains(line, edited[0].BaseCommit) && !strings.Contains(line, edited[0].SolutionCommit) {
			t.Errorf("git was asked about another task: %s", line)
		}
	}
	// The edited task's old answer is gone from the file, and its new one is there.
	kept = OpenGapsCache(path, "build", "git")
	if _, ok := kept.lookup(unfair); ok {
		t.Error("the answer of the task as it was is still in the file")
	}
	if now, _ := inputOf(edited[0]); func() bool { _, ok := kept.lookup(now); return !ok }() {
		t.Error("the edited task's answer is not in the file")
	}
}

// Only what a later check would find again goes into the file: not a check that failed, not one that skipped a file
// it could not read, not one of a cancelled command, not one whose commits are named by branches, and no text that
// the file could not give back as it is.
func TestGapsCacheKeepsOnlyCompleteAnswersOfPinnedCommits(t *testing.T) {
	repo, tasks := gapTasks(t)
	want := gapsAlone(t, repo, tasks)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gaps.json")
	open := func() (*Fairness, *GapsCache) {
		kept := OpenGapsCache(path, "build", "git")
		f := NewFairness("-C", repo)
		f.Keep(kept)
		return f, kept
	}
	stored := func(kept *GapsCache, tk store.Task) bool {
		t.Helper()
		if err := kept.Save(append(append([]store.Task{}, tasks...), tk)); err != nil {
			t.Fatal(err)
		}
		in, _ := inputOf(tk)
		_, ok := OpenGapsCache(path, "build", "git").lookup(in)
		return ok
	}

	// A failed check.
	broken := tasks[0]
	broken.SolutionCommit = strings.Repeat("0", 40)
	f, kept := open()
	if _, err := Gaps(ctx, f, broken); err == nil || stored(kept, broken) {
		t.Errorf("a failed check: error %v, stored %v", err, err == nil)
	}

	// A check that skipped a file it could not read: the answer is given, incomplete, and not kept.
	field := tasks[2]
	blob := git(t, repo, "rev-parse", field.SolutionCommit+":p/p.go")
	object := filepath.Join(repo, ".git", "objects", blob[:2], blob[2:])
	if err := os.Rename(object, object+".away"); err != nil {
		t.Fatal(err)
	}
	f, kept = open()
	gaps, err := Gaps(ctx, f, field)
	if err := os.Rename(object+".away", object); err != nil {
		t.Fatal(err)
	}
	if err != nil || len(gaps) != 0 || stored(kept, field) {
		t.Errorf("a check with an unreadable file: %v, %v; stored: its incomplete answer must not be", gaps, err)
	}
	if got := listed(t, repo, path, "build", "git", tasks); !reflect.DeepEqual(got[2], want[2]) {
		t.Errorf("the next list's answer: %v, want %v", got[2], want[2])
	}

	// A cancelled command.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	f, kept = open()
	PrepareGaps(cancelled, f, tasks)
	if _, err := Gaps(cancelled, f, tasks[0]); err == nil || stored(kept, tasks[0]) {
		t.Errorf("a cancelled check: error %v; it must fail and store nothing", err)
	}
	overtakenCtx := &overtaken{Context: ctx}
	f, kept = open()
	if _, err := Gaps(overtakenCtx, f, tasks[4]); err == nil || stored(kept, tasks[4]) {
		t.Errorf("a check a cancel overtook: error %v; it must fail and store nothing", err)
	}

	// Commits named by branches: checked as ever, never kept, since a branch can move.
	git(t, repo, "branch", "the-base", field.BaseCommit)
	git(t, repo, "branch", "the-solution", field.SolutionCommit)
	named := field
	named.BaseCommit, named.SolutionCommit = "the-base", "the-solution"
	f, kept = open()
	if gaps, err := Gaps(ctx, f, named); err != nil || !reflect.DeepEqual(gaps, want[2]) || stored(kept, named) {
		t.Errorf("a task named by branches: %v, %v; its answer must be given and not stored", gaps, err)
	}
	half := field
	half.BaseCommit = "the-base"
	f, kept = open()
	if gaps, err := Gaps(ctx, f, half); err != nil || !reflect.DeepEqual(gaps, want[2]) || stored(kept, half) {
		t.Errorf("a task whose base is named by a branch: %v, %v; its answer must be given and not stored", gaps, err)
	}

	// A text that is not UTF-8 would come back changed from the file.
	_, kept = open()
	in, _ := inputOf(tasks[1])
	kept.store(in, []Gap{{Kind: GapLiteral, Text: "bad \xff text", File: "a_test.go"}})
	if _, ok := kept.lookup(in); ok {
		t.Error("a gap whose text is not UTF-8 was kept")
	}
}

// A file that cannot be used counts as empty and is replaced: a cut one, one of another format, anything else.
func TestGapsCacheIgnoresAFileItCannotUse(t *testing.T) {
	repo, tasks := gapTasks(t)
	want := gapsAlone(t, repo, tasks)
	path := filepath.Join(t.TempDir(), "gaps.json")
	listed(t, repo, path, "build", "git", tasks)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file gapsFile
	if err := json.Unmarshal(good, &file); err != nil {
		t.Fatal(err)
	}
	file.Format = gapsFormat + 1
	newer, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := inputOf(tasks[0])
	for name, content := range map[string][]byte{
		"a cut file":             good[:len(good)/2],
		"another format":         newer,
		"not JSON":               []byte("gaps\n"),
		"an empty file":          nil,
		"JSON of another shape":  []byte(`["format", 1]`),
		"a file without answers": []byte(`{"format":1,"checkers":[{"checker":"` + digest("build", "git") + `"}]}`),
		"the first layout tried": []byte(`{"format":1,"checker":"` + digest("build", "git") + `","gaps":{}}`),
	} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if gaps, ok := OpenGapsCache(path, "build", "git").lookup(in); ok {
			t.Errorf("%s gave %v", name, gaps)
		}
		if got := listed(t, repo, path, "build", "git", tasks); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the list gave %v, want %v", name, got, want)
		}
		if again, err := os.ReadFile(path); err != nil || !bytes.Equal(again, good) {
			t.Errorf("%s was not replaced by a good file (%v)", name, err)
		}
	}
}

// The file holds the answers of the project's tasks as they are now, and no others; and a save that cannot write says
// so, with the file's place.
func TestGapsCacheDropsWhatNoTaskHasAndReportsAFailedSave(t *testing.T) {
	repo, tasks := gapTasks(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "gaps.json")
	listed(t, repo, path, "build", "git", tasks)
	listed(t, repo, path, "another build", "git", tasks)
	listed(t, repo, path, "build", "git", tasks[:2])           // three tasks were removed
	for _, build := range []string{"build", "another build"} { // dropped for every checker, not only the one that saved
		kept := OpenGapsCache(path, build, "git")
		for i, tk := range tasks {
			in, _ := inputOf(tk)
			if _, ok := kept.lookup(in); ok != (i < 2) {
				t.Errorf("%s, %s: in the file %v, want %v", build, tk.Name, ok, i < 2)
			}
		}
	}
	// Nothing to check keeps the file as it is: a task without a solution has no answer.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	listed(t, repo, path, "build", "git", append(tasks[:2:2], store.Task{Name: "unsolved", BaseCommit: tasks[0].BaseCommit}))
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the file changed for a task with nothing to check (%v)", err)
	}

	blocked := filepath.Join(dir, "a-file", "gaps.json") // its folder cannot be made: a file is in the way
	if err := os.WriteFile(filepath.Dir(blocked), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stuck := OpenGapsCache(blocked, "build", "git")
	f := NewFairness("-C", repo)
	f.Keep(stuck)
	if gaps, err := Gaps(context.Background(), f, tasks[0]); err != nil || len(gaps) != 3 {
		t.Fatalf("a check whose answer cannot be kept: %v, %v", gaps, err)
	}
	if err := stuck.Save(tasks); err == nil || !strings.Contains(err.Error(), blocked) {
		t.Errorf("a save that cannot write: %v, want an error naming %s", err, blocked)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Errorf("files left behind: %v", left)
	}
}

// digest tells lists of texts apart wherever they are cut.
func TestDigestTellsTextsApart(t *testing.T) {
	seen := map[string]string{}
	for _, texts := range [][]string{{}, {""}, {"", ""}, {"ab"}, {"a", "b"}, {"", "ab"}, {"ab", ""}, {"1\x00a"}, {"a\x001\x00b"}} {
		d := digest(texts...)
		if len(d) != 64 {
			t.Errorf("digest(%q) = %q", texts, d)
		}
		if earlier, ok := seen[d]; ok {
			t.Errorf("%q and %s share a digest", texts, earlier)
		}
		seen[d] = strings.Join(texts, "|")
	}
	if digest("a", "b") != digest("a", "b") {
		t.Error("equal texts have different digests")
	}
}
