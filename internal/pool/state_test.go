package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pigeaca/agentium/internal/store"
)

func load(t *testing.T, file string) State {
	t.Helper()
	st, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestLoadMissingUnreadableAndLeftovers(t *testing.T) {
	dir := t.TempDir()
	file := StateFile(filepath.Join(dir, "repo.git"))
	if file != filepath.Join(dir, "pool.json") {
		t.Errorf("StateFile = %s", file)
	}
	if st := load(t, file); st.Unreadable != "" || len(st.Watermark) != 0 {
		t.Errorf("missing file: %+v", st)
	}
	// A kill between the temporary file and the rename leaves the temporary file, which nothing reads.
	if err := os.WriteFile(filepath.Join(dir, "pool-123.tmp"), []byte("{half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(file, func(st *State) error { st.Watermark = []string{"abc"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if st := load(t, file); !slices.Equal(st.Watermark, []string{"abc"}) || st.Unreadable != "" {
		t.Errorf("after Update: %+v", st)
	}
	for name, data := range map[string][]byte{"corrupt": []byte("{not json"), "too large": []byte(`{"dismissed":["` + strings.Repeat("a", maxState) + `"]}`)} {
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if st := load(t, file); st.Unreadable == "" || len(st.Watermark) != 0 {
			t.Errorf("%s: %+v", name, st)
		}
	}
}

// An unreadable file is set aside under a name of its own before the next write replaces it; a failing change writes
// and moves nothing.
func TestAnUnreadableFileIsSetAside(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "pool.json")
	if err := os.WriteFile(file, []byte(`{"dismissed":["c1"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(file, func(st *State) error { return fmt.Errorf("no") }); err == nil {
		t.Error("a failing change must fail Update")
	}
	if aside, _ := filepath.Glob(file + ".corrupt-*"); len(aside) != 0 || load(t, file).Unreadable == "" {
		t.Errorf("a failing change touched the file: %v", aside)
	}
	st, err := Update(file, func(st *State) error {
		if st.Unreadable == "" {
			t.Error("the change must see that the file was unreadable")
		}
		st.Watermark = []string{"def"}
		return nil
	})
	if err != nil || !slices.Equal(st.Watermark, []string{"def"}) || st.Unreadable != "" || !slices.Equal(load(t, file).Watermark, []string{"def"}) {
		t.Errorf("replacing an unreadable file: %+v, %v", st, err)
	}
	aside, _ := filepath.Glob(file + ".corrupt-*")
	if len(aside) != 1 {
		t.Fatalf("set aside: %v", aside)
	}
	if data, err := os.ReadFile(aside[0]); err != nil || string(data) != `{"dismissed":["c1"` {
		t.Errorf("the set-aside file holds %q, %v", data, err)
	}
}

// A state file that cannot be read for another reason than its content is an error, and Update writes nothing over
// it: the file may be fine.
func TestALoadErrorIsNotReplaced(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	if err := os.Mkdir(file, 0o700); err != nil { // reading a folder fails: an I/O error, not a parse error
		t.Fatal(err)
	}
	if _, err := Load(file); err == nil {
		t.Error("Load of an unreadable path must fail")
	}
	called := false
	if _, err := Update(file, func(*State) error { called = true; return nil }); err == nil || called {
		t.Errorf("Update = %v, change called %v", err, called)
	}
	if info, err := os.Stat(file); err != nil || !info.IsDir() {
		t.Errorf("the path was replaced: %v, %v", info, err)
	}
	if aside, _ := filepath.Glob(file + ".corrupt-*"); len(aside) != 0 {
		t.Errorf("set aside: %v", aside)
	}
}

// Updates from many writers at once keep every change: each reads the file under the lock.
func TestConcurrentUpdatesKeepEveryChange(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pool.json")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Update(file, func(st *State) error {
				st.Dismissed = append(st.Dismissed, fmt.Sprintf("c%02d", i))
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := load(t, file).Dismissed; len(got) != 20 {
		t.Errorf("dismissed %d of 20: %v", len(got), got)
	}
}

func TestReconcile(t *testing.T) {
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	tasks := []store.Task{
		{Name: "kept", SolutionCommit: "c1", CreatedAt: at},
		{Name: "reused", SolutionCommit: "c9", CreatedAt: at.Add(time.Hour)}, // the name of a removed mined task, now another task
		{Name: "imported", SolutionCommit: "c3", CreatedAt: at},
		{Name: "orphan", SolutionCommit: "c4", CreatedAt: at},
	}
	st := State{
		Mined: []Record{
			{Name: "kept", SolutionCommit: "c1", Patch: "p1", CreatedAt: at},
			{Name: "removed", SolutionCommit: "c2", Patch: "p2", CreatedAt: at},
			{Name: "reused", SolutionCommit: "c5", CreatedAt: at},
		},
		Pending:   []Pending{{Commit: "c3", Patch: "p3"}, {Commit: "c4", Patch: "p4"}, {Commit: "c6"}},
		Dismissed: []string{"c0"},
	}
	st.Reconcile(tasks, tasks[2:3])
	st.Reconcile(tasks, tasks[2:3]) // again: nothing changes
	want := []Record{
		{Name: "kept", SolutionCommit: "c1", Patch: "p1", CreatedAt: at},
		{Name: "imported", SolutionCommit: "c3", Patch: "p3", CreatedAt: at},
		{Name: "orphan", SolutionCommit: "c4", Patch: "p4", CreatedAt: at, Recovered: true},
	}
	if !slices.Equal(st.Mined, want) || len(st.Pending) != 0 || !slices.Equal(st.Dismissed, []string{"c0", "c2", "c5"}) ||
		!slices.Equal(st.DismissedPatches, []string{"p2"}) {
		t.Errorf("Reconcile = %+v", st)
	}
	tasks[0].Validation = []byte(`{"status":"valid"}`)
	tasks[3].RetiredAt = at
	var names []string
	for _, tk := range st.Unvalidated(tasks) {
		names = append(names, tk.Name)
	}
	if !slices.Equal(names, []string{"imported"}) {
		t.Errorf("Unvalidated = %v (mined, active, without a validation)", names)
	}
}
