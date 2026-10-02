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

func TestLoadMissingUnreadableAndLeftovers(t *testing.T) {
	dir := t.TempDir()
	file := StateFile(filepath.Join(dir, "repo.git"))
	if file != filepath.Join(dir, "pool.json") {
		t.Errorf("StateFile = %s", file)
	}
	if st := Load(file); st.Unreadable != "" || st.Watermark != "" {
		t.Errorf("missing file: %+v", st)
	}
	// A kill between the temporary file and the rename leaves the temporary file, which nothing reads.
	if err := os.WriteFile(filepath.Join(dir, "pool-123.tmp"), []byte("{half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(file, func(st *State) error { st.Watermark = "abc"; return nil }); err != nil {
		t.Fatal(err)
	}
	if st := Load(file); st.Watermark != "abc" || st.Unreadable != "" {
		t.Errorf("after Update: %+v", st)
	}
	for name, data := range map[string][]byte{"corrupt": []byte("{not json"), "too large": []byte(`{"dismissed":["` + strings.Repeat("a", maxState) + `"]}`)} {
		if err := os.WriteFile(file, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if st := Load(file); st.Unreadable == "" || st.Watermark != "" {
			t.Errorf("%s: %+v", name, st)
		}
	}
	// The next write replaces an unreadable file; a failing change writes nothing.
	if _, err := Update(file, func(st *State) error { return fmt.Errorf("no") }); err == nil {
		t.Error("a failing change must fail Update")
	}
	if st := Load(file); st.Unreadable == "" {
		t.Error("a failing change rewrote the file")
	}
	st, err := Update(file, func(st *State) error {
		if st.Unreadable == "" {
			t.Error("the change must see that the file was unreadable")
		}
		st.Watermark = "def"
		return nil
	})
	if err != nil || st.Watermark != "def" || st.Unreadable != "" || Load(file).Watermark != "def" {
		t.Errorf("replacing an unreadable file: %+v, %v", st, err)
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
	if got := Load(file).Dismissed; len(got) != 20 {
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
			{Name: "kept", SolutionCommit: "c1", CreatedAt: at},
			{Name: "removed", SolutionCommit: "c2", CreatedAt: at},
			{Name: "reused", SolutionCommit: "c5", CreatedAt: at},
		},
		Pending:   []string{"c3", "c4", "c6"},
		Dismissed: []string{"c0"},
	}
	st.Reconcile(tasks, tasks[2:3])
	st.Reconcile(tasks, tasks[2:3]) // again: nothing changes
	want := []Record{
		{Name: "kept", SolutionCommit: "c1", CreatedAt: at},
		{Name: "imported", SolutionCommit: "c3", CreatedAt: at},
		{Name: "orphan", SolutionCommit: "c4", CreatedAt: at, Recovered: true},
	}
	if !slices.Equal(st.Mined, want) || len(st.Pending) != 0 || !slices.Equal(st.Dismissed, []string{"c0", "c2", "c5"}) {
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
