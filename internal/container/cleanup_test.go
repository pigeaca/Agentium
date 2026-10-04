package container

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// Cleanup never removes what is in use: a running container, a volume a container mounts (or whose users cannot be
// read), or anything made within the grace; leftovers go, and a deps volume once unused for the age given.
func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-48*time.Hour), now.Add(-10*time.Minute)
	items := []Item{
		{Kind: "container", Name: "c-running", State: "running", Created: old},
		{Kind: "container", Name: "c-paused", State: "paused", Created: old},
		{Kind: "container", Name: "c-removing", State: "removing", Created: old},
		{Kind: "container", Name: "c-created-now", State: "created", Created: recent},
		{Kind: "container", Name: "c-created-old", State: "created", Created: old},
		{Kind: "container", Name: "c-exited", State: "exited", Created: old},
		{Kind: "container", Name: "c-unknown-age", State: "exited"},
		{Kind: "volume", Name: "v-grade-used", Run: "r1", Users: 1, Created: old},
		{Kind: "volume", Name: "v-grade-orphan", Run: "r1", Users: 0, Created: old},
		{Kind: "volume", Name: "v-grade-new", Run: "r1", Users: 0, Created: recent},
		{Kind: "volume", Name: "v-users-unknown", Run: "r1", Users: -1, Created: old},
		{Kind: "volume", Name: "v-deps-mounted", Deps: "3", Users: 2, Created: old},
		{Kind: "volume", Name: "v-deps-used-lately", Deps: "3", Users: 0, Created: old},
		{Kind: "volume", Name: "v-deps-stale", Deps: "3", Users: 0, Created: old},
		{Kind: "volume", Name: "v-deps-unmarked", Deps: "3", Users: 0, Created: now.Add(-40 * 24 * time.Hour)},
		// A volume whose only container goes, goes after it; one with a container that stays, stays.
		{Kind: "volume", Name: "v-of-leftover", Run: "r2", Users: 1, UsedBy: []string{"c-exited"}, Created: old},
		{Kind: "volume", Name: "v-of-two", Deps: "3", Users: 2, UsedBy: []string{"c-exited", "c-running"}, Created: old},
	}
	lastUse := func(name string) time.Time {
		switch name {
		case "v-deps-used-lately":
			return now.Add(-5 * 24 * time.Hour)
		case "v-deps-stale":
			return now.Add(-31 * 24 * time.Hour)
		}
		return time.Time{}
	}
	got := map[string]string{}
	for _, d := range Decide(items, now, time.Hour, 30*24*time.Hour, lastUse) {
		got[d.Name] = map[bool]string{true: "remove", false: "keep"}[d.Remove] + " " + d.Reason
	}
	want := map[string]string{
		"c-running": "keep running", "c-paused": "keep running", "c-removing": "keep running", "c-created-now": "keep recently_used",
		"c-created-old": "remove leftover", "c-exited": "remove leftover", "c-unknown-age": "keep recently_used",
		"v-grade-used": "keep in_use", "v-grade-orphan": "remove leftover", "v-grade-new": "keep recently_used", "v-users-unknown": "keep in_use",
		"v-deps-mounted": "keep in_use", "v-deps-used-lately": "keep recently_used", "v-deps-stale": "remove unused", "v-deps-unmarked": "remove unused",
		"v-of-leftover": "remove leftover", "v-of-two": "keep in_use",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: %q, want %q", name, got[name], w)
		}
	}
}

// The inventory is the data folder's labelled containers and volumes, with each volume's users and size.
func TestInventory(t *testing.T) {
	t.Parallel()
	sc := goodScenario(t)
	sc.PS = "agentium-test-r1-grade\tcreated\tr1\t2026-10-04 11:00:00 +0400 +04\nagentium-test-seed-ab-seed\texited\tseed-ab\t2026-10-01 09:00:00 +0000 UTC\n"
	sc.Volumes = strings.Repeat("a", 64) + "\tr1\t\nagentium-deps-test-3-84349ebd3bf9\t\t3\n"
	sc.VolumeCreated = strings.Repeat("a", 64) + "\t2026-10-04T07:00:00Z\nagentium-deps-test-3-84349ebd3bf9\t2026-09-01T00:00:00Z\n"
	sc.DF = `[{"Name":"` + strings.Repeat("a", 64) + `","Links":"1","Size":"12MB"},{"Name":"agentium-deps-test-3-84349ebd3bf9","Links":"0","Size":"1.5GB"},{"Name":"other","Links":"3","Size":"1GB"}]`
	sc.VolumeUsersBy = map[string]string{strings.Repeat("a", 64): "agentium-test-r1-grade\n"}
	f, d := openFake(t, sc)
	items, err := d.Inventory(context.Background(), "test")
	must(t, err)
	var got []string
	for _, it := range items {
		got = append(got, it.Kind+" "+it.Name+" "+it.State+" run="+it.Run+" deps="+it.Deps+" users="+itoa(it.Users)+" size="+itoa(int(it.Size))+" "+it.Created.UTC().Format(time.RFC3339))
	}
	want := []string{
		"container agentium-test-r1-grade created run=r1 deps= users=-1 size=-1 2026-10-04T07:00:00Z",
		"container agentium-test-seed-ab-seed exited run=seed-ab deps= users=-1 size=-1 2026-10-01T09:00:00Z",
		"volume " + strings.Repeat("a", 64) + "  run=r1 deps= users=1 size=12000000 2026-10-04T07:00:00Z",
		"volume agentium-deps-test-3-84349ebd3bf9  run= deps=3 users=0 size=1500000000 2026-09-01T00:00:00Z",
	}
	if !slices.Equal(got, want) {
		t.Errorf("items:\n%q\nwant\n%q", got, want)
	}
	for _, argv := range f.calls(t) {
		a := strings.Join(argvAfter(argv), " ")
		// A volume's users are every container that mounts it, whoever made it: one of another's keeps it.
		if (strings.HasPrefix(a, "ps") && !strings.Contains(a, "volume=") || strings.HasPrefix(a, "volume ls")) && (!strings.Contains(a, "label=agentium.data=test") || !strings.Contains(a, "label=agentium.mode=container-v1")) {
			t.Errorf("listed beyond the data folder: %s", a)
		}
	}
	if items[2].UsedBy[0] != "agentium-test-r1-grade" {
		t.Errorf("used by %v", items[2].UsedBy)
	}
	// Sizes that cannot be read are unknown, and nothing else changes.
	sc.DF = "not json"
	f.set(t, sc)
	items, err = d.Inventory(context.Background(), "test")
	must(t, err)
	for _, it := range items {
		if it.Kind == "volume" && it.Size != -1 {
			t.Errorf("%s: size %d without df", it.Name, it.Size)
		}
	}
	if _, err := d.Inventory(context.Background(), "../x"); err == nil {
		t.Error("a bad data ID accepted")
	}
}

// RemoveIdle reads each item again and removes it only when idle and the data folder's, never forcing.
func TestRemoveIdle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	container := Item{Kind: "container", Name: "agentium-test-r1-grade"}
	volume := Item{Kind: "volume", Name: "agentium-deps-test-3-84349ebd3bf9"}
	for _, tt := range []struct {
		name    string
		item    Item
		change  func(*scenario)
		inUse   bool
		fail    bool
		removed []string
	}{
		{"an exited container", container, func(sc *scenario) { sc.IdleState = "exited\ttest" }, false, false, []string{"rm", "--volumes", container.Name}},
		{"a created container", container, func(sc *scenario) { sc.IdleState = "created\ttest" }, false, false, []string{"rm", "--volumes", container.Name}},
		{"a container that started since", container, func(sc *scenario) { sc.IdleState = "running\ttest" }, true, true, nil},
		{"another data folder's", container, func(sc *scenario) { sc.IdleState = "exited\tother" }, false, true, nil},
		{"a container gone", container, func(sc *scenario) { sc.IdleState = "" }, false, false, nil},
		{"a container the daemon refuses", container, func(sc *scenario) {
			sc.IdleState = "exited\ttest"
			sc.RmRefuse = "Error response from daemon: cannot remove container: container is running: stop the container before removing or force remove"
		}, true, true, []string{"rm", "--volumes", container.Name}},
		{"an unused volume", volume, func(sc *scenario) { sc.VolumeData = "test" }, false, false, []string{"volume", "rm", volume.Name}},
		{"a volume mounted since", volume, func(sc *scenario) { sc.VolumeData, sc.VolumeUsers = "test", strings.Repeat("c", 64)+"\n" }, true, true, nil},
		{"another data folder's volume", volume, func(sc *scenario) { sc.VolumeData = "other" }, false, true, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sc := goodScenario(t)
			tt.change(&sc)
			f, d := openFake(t, sc)
			err := d.RemoveIdle(ctx, "test", tt.item)
			if (err != nil) != tt.fail || errors.Is(err, ErrInUse) != tt.inUse {
				t.Errorf("RemoveIdle: %v (want failure %v, in use %v)", err, tt.fail, tt.inUse)
			}
			var removals [][]string
			for _, argv := range f.calls(t) {
				a := argvAfter(argv)
				if a[0] == "rm" || len(a) > 1 && a[0] == "volume" && a[1] == "rm" {
					removals = append(removals, a)
					if slices.Contains(a, "--force") {
						t.Errorf("forced: %v", a)
					}
				}
			}
			if tt.removed == nil && len(removals) > 0 || tt.removed != nil && (len(removals) != 1 || !slices.Equal(removals[0], tt.removed)) {
				t.Errorf("removals %v, want %v", removals, tt.removed)
			}
		})
	}
}
