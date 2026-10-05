package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrInUse: a container or volume that cleanup was asked to remove is in use now (a container running, a volume that a
// container mounts), so it stays.
var ErrInUse = errors.New("in use")

// Item is a container or a volume of a data folder on the daemon, found by its labels.
type Item struct {
	Kind    string // "container" or "volume"
	Name    string
	Run     string    // its agentium.run label: a grade's run, or a warm-up's, a seed's or an image check's; "" for a deps volume
	State   string    // a container's: created, running, paused, restarting, removing, exited, dead
	Created time.Time // zero when unknown
	Deps    string    // a deps volume's project; "" otherwise
	Users   int       // a volume's containers, in any state; -1 when unknown (then it is in use)
	UsedBy  []string  // their names
	Size    int64     // a volume's size in bytes; -1 when unknown
}

// Inventory lists a data folder's containers (in any state, created and never started included) and labelled volumes
// (the grades' anonymous ones and the deps volumes), with how many containers use each volume. Nothing else on the
// daemon is listed.
func (d *Docker) Inventory(ctx context.Context, data string) ([]Item, error) {
	if !dataPattern.MatchString(data) {
		return nil, fmt.Errorf("inventory: data ID %q", data)
	}
	filters := []string{"--filter", "label=" + LabelData + "=" + data, "--filter", "label=" + LabelMode + "=" + Mode}
	out, err := d.output(ctx, append([]string{"ps", "--all", "--no-trunc", "--format", `{{.Names}}	{{.State}}	{{.Label "agentium.run"}}	{{.CreatedAt}}`}, filters...)...)
	if err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	var items []Item
	for _, line := range lines(out) {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		items = append(items, Item{Kind: "container", Name: f[0], State: f[1], Run: f[2], Created: psTime(f[3]), Users: -1, Size: -1})
	}
	out, err = d.output(ctx, append([]string{"volume", "ls", "--format", `{{.Name}}	{{.Label "agentium.run"}}	{{.Label "agentium.deps"}}`}, filters...)...)
	if err != nil {
		return nil, fmt.Errorf("inventory: %w", err)
	}
	var names []string
	for _, line := range lines(out) {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		items = append(items, Item{Kind: "volume", Name: f[0], Run: f[1], Deps: f[2], Users: -1, Size: -1})
		names = append(names, f[0])
	}
	if len(names) == 0 {
		return items, nil
	}
	created := map[string]time.Time{}
	if out, err := d.output(ctx, append([]string{"volume", "inspect", "--format", "{{.Name}}\t{{.CreatedAt}}"}, names...)...); err == nil {
		for _, line := range lines(out) {
			if name, at, ok := strings.Cut(line, "\t"); ok {
				if t, err := time.Parse(time.RFC3339, strings.TrimSpace(at)); err == nil {
					created[name] = t
				}
			}
		}
	}
	// Sizes come from one call, and are only shown; a volume's users from its own (one that fails leaves it in use).
	var df []struct{ Name, Size string }
	if out, err := d.output(ctx, "system", "df", "--verbose", "--format", "{{json .Volumes}}"); err == nil {
		_ = json.Unmarshal(bytes.TrimSpace(out), &df)
	}
	for i := range items {
		if items[i].Kind != "volume" {
			continue
		}
		items[i].Created = created[items[i].Name]
		for _, v := range df {
			if v.Name == items[i].Name {
				items[i].Size = HumanSize(v.Size)
			}
		}
		if out, err := d.output(ctx, "ps", "--all", "--no-trunc", "--filter", "volume="+items[i].Name, "--format", "{{.Names}}"); err == nil {
			items[i].UsedBy = lines(out)
			items[i].Users = len(items[i].UsedBy)
		}
	}
	return items, nil
}

func lines(out []byte) []string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// psTime reads docker ps's CreatedAt ("2026-10-04 12:00:00 +0400 +04"); zero when it cannot.
func psTime(s string) time.Time {
	if len(s) >= 25 {
		if t, err := time.Parse("2006-01-02 15:04:05 -0700", s[:25]); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Why an item is kept or removed (Decision.Reason).
const (
	ReasonRunning  = "running"       // a running container: its own deadline removes it (--rm)
	ReasonRecent   = "recently_used" // made, or a deps volume used, within the grace: a command may be using it now
	ReasonInUse    = "in_use"        // a volume a container mounts, or whose users cannot be read
	ReasonLeftover = "leftover"      // a container no command runs, or a volume without its container
	ReasonUnused   = "unused"        // a deps volume nothing has used for the age given
)

// Decision is what cleanup does with one item.
type Decision struct {
	Item
	Remove bool
	Reason string
	Detail string
	// LastUsed is a deps volume's last use (lastUse, or its creation when unknown); zero when unknown.
	LastUsed time.Time
}

// Decide plans the cleanup of a data folder's items: never one in use (a running container, a volume a container
// mounts or whose users are unknown), and nothing made within grace (a command between create and start). Other
// containers and the grades' volumes are leftovers and go. A deps volume goes once nothing has used it for olderThan;
// lastUse (nil, or a zero time: unknown) is its last use as the caller recorded it, else its creation. A volume whose
// only users are containers that go is decided as if they were gone: it is removed after them (RemoveIdle checks
// again).
func Decide(items []Item, now time.Time, grace, olderThan time.Duration, lastUse func(volume string) time.Time) []Decision {
	var containers []Decision
	going := map[string]bool{}
	for _, it := range items {
		if it.Kind == "container" {
			d := decide(it, now, grace, olderThan, lastUse)
			containers = append(containers, d)
			going[it.Name] = d.Remove
		}
	}
	out := containers
	for _, it := range items {
		if it.Kind == "container" {
			continue
		}
		if it.Users > 0 && len(it.UsedBy) == it.Users && !slices.ContainsFunc(it.UsedBy, func(name string) bool { return !going[name] }) {
			it.Users, it.UsedBy = 0, nil // its containers go first
		}
		out = append(out, decide(it, now, grace, olderThan, lastUse))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func decide(it Item, now time.Time, grace, olderThan time.Duration, lastUse func(volume string) time.Time) Decision {
	d := Decision{Item: it}
	{
		switch {
		case it.Kind == "container" && it.State != "created" && it.State != "exited" && it.State != "dead":
			d.Reason, d.Detail = ReasonRunning, "a container that is "+it.State+": its own deadline removes it"
		case it.Kind == "container" && (it.Created.IsZero() || now.Sub(it.Created) < grace):
			d.Reason, d.Detail = ReasonRecent, "made within the last "+span(grace)+": a command may be about to start it"
		case it.Kind == "container":
			d.Remove, d.Reason, d.Detail = true, ReasonLeftover, "a "+it.State+" container that no command runs"
		case it.Users != 0:
			d.Reason, d.Detail = ReasonInUse, usersText(it.Users)
		case it.Deps == "" && (it.Created.IsZero() || now.Sub(it.Created) < grace):
			d.Reason, d.Detail = ReasonRecent, "made within the last "+span(grace)
		case it.Deps == "":
			d.Remove, d.Reason, d.Detail = true, ReasonLeftover, "a grade's volume whose container is gone"
		default:
			last := it.Created
			if lastUse != nil {
				if t := lastUse(it.Name); !t.IsZero() {
					last = t
				}
			}
			d.LastUsed = last
			age := now.Sub(last)
			switch {
			case last.IsZero() || age < grace:
				d.Reason, d.Detail = ReasonRecent, "used within the last "+span(grace)
			case age < olderThan:
				d.Reason, d.Detail = ReasonRecent, "dependencies of project "+it.Deps+", used "+days(age)+" ago"
			default:
				d.Remove, d.Reason, d.Detail = true, ReasonUnused, "dependencies of project "+it.Deps+", unused for "+days(age)
			}
		}
	}
	return d
}

func usersText(n int) string {
	switch {
	case n < 0:
		return "in use, as far as can be told (its containers cannot be read)"
	case n == 1:
		return "in use by a container"
	}
	return "in use by " + strconv.Itoa(n) + " containers"
}

// span is a grace period for people: "hour", "2h0m0s".
func span(d time.Duration) string {
	if d == time.Hour {
		return "hour"
	}
	return d.String()
}

func days(d time.Duration) string {
	if d >= 48*time.Hour {
		return strconv.Itoa(int(d/(24*time.Hour))) + " days"
	}
	return d.Round(time.Minute).String()
}

// RemoveIdle removes one container or volume of the data folder, read again first: a container only when it carries
// the data folder's label and is not running (removed without force, so the daemon refuses one that started since), a
// volume only when it carries the label and no container uses it (removed without force, so the daemon refuses one a
// container mounts since). An item already gone is not an error; one in use is ErrInUse.
func (d *Docker) RemoveIdle(ctx context.Context, data string, it Item) error {
	if !dataPattern.MatchString(data) {
		return fmt.Errorf("remove: data ID %q", data)
	}
	switch it.Kind {
	case "container":
		out, stderr, res, err := d.call(ctx, []string{"container", "inspect", "--format", `{{.State.Status}}	{{index .Config.Labels "agentium.data"}}`, it.Name}, nil, controlTimeout)
		if err != nil {
			return fmt.Errorf("remove %s: %w", it.Name, err)
		}
		if res.ExitCode != 0 {
			if noSuchContainer(stderr) {
				return nil
			}
			return fmt.Errorf("remove %s: %s", it.Name, firstLine(stderr))
		}
		state, label, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
		switch {
		case label != data:
			return fmt.Errorf("remove %s: not this data folder's container", it.Name)
		case state != "created" && state != "exited" && state != "dead":
			return fmt.Errorf("remove %s: %w (%s)", it.Name, ErrInUse, state)
		}
		_, stderr, res, err = d.call(ctx, []string{"rm", "--volumes", it.Name}, nil, removeTimeout)
		if err != nil {
			return fmt.Errorf("remove %s: %w", it.Name, err)
		}
		if res.ExitCode != 0 && !noSuchContainer(stderr) {
			if strings.Contains(stderr, "running") || strings.Contains(stderr, "stop the container") {
				return fmt.Errorf("remove %s: %w: %s", it.Name, ErrInUse, firstLine(stderr))
			}
			return fmt.Errorf("remove %s: %s", it.Name, firstLine(stderr))
		}
		return nil
	case "volume":
		out, stderr, res, err := d.call(ctx, []string{"volume", "inspect", "--format", `{{index .Labels "agentium.data"}}`, it.Name}, nil, controlTimeout)
		if err != nil {
			return fmt.Errorf("remove %s: %w", it.Name, err)
		}
		if res.ExitCode != 0 {
			if strings.Contains(strings.ToLower(stderr), "no such volume") {
				return nil
			}
			return fmt.Errorf("remove %s: %s", it.Name, firstLine(stderr))
		}
		if strings.TrimSpace(string(out)) != data {
			return fmt.Errorf("remove %s: not this data folder's volume", it.Name)
		}
		users, err := d.output(ctx, "ps", "--all", "--quiet", "--no-trunc", "--filter", "volume="+it.Name)
		if err != nil {
			return fmt.Errorf("remove %s: %w", it.Name, err)
		}
		if n := len(lines(users)); n > 0 {
			return fmt.Errorf("remove %s: %w (%s)", it.Name, ErrInUse, usersText(n))
		}
		_, stderr, res, err = d.call(ctx, []string{"volume", "rm", it.Name}, nil, removeTimeout)
		if err != nil {
			return fmt.Errorf("remove %s: %w", it.Name, err)
		}
		if res.ExitCode != 0 && !strings.Contains(strings.ToLower(stderr), "no such volume") {
			if strings.Contains(stderr, "in use") {
				return fmt.Errorf("remove %s: %w: %s", it.Name, ErrInUse, firstLine(stderr))
			}
			return fmt.Errorf("remove %s: %s", it.Name, firstLine(stderr))
		}
		return nil
	}
	return fmt.Errorf("remove %s: unknown kind %q", it.Name, it.Kind)
}
