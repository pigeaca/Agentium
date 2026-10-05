package container

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files (the argv golden, and the inspect and probe fixtures from a real daemon)")

// fakeDockerName is the name the test binary runs under as the fake docker client (see TestMain): a link to the test
// binary, so the fake sees exactly the argv and environment the driver gives docker.
const fakeDockerName = "docker"

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == fakeDockerName {
		// syscall.Exit, not os.Exit: under -race, os.Exit waits a second for the race runtime (GORACE atexit_sleep_ms),
		// which the client's allowlisted environment cannot turn off, and the fake runs a hundred times.
		syscall.Exit(fakeDocker())
	}
	os.Exit(m.Run())
}

// scenario is what the fake docker answers, read from scenario.json in the folder above its own bin/ (found from its
// path: after the endpoint lookup the driver passes docker no variable but PATH). Every call is appended to calls there
// as one JSON array of its argv after the binary, and what it reads from stdin is kept in stdin-<n>. A file named gone
// there means the container no longer exists.
type scenario struct {
	Endpoint     string // the context's endpoint
	ContextFail  string // the endpoint lookup fails with this message
	Version      string // docker version --format {{json .Server}}
	Info         string // docker info --format {{json .}}
	Image        string // docker image inspect --format {{json .}}; empty: no such image
	Inspect      string // docker container inspect; empty: no such container
	Volume       string // docker volume inspect --format {{json .}}; empty: no such volume
	Probe        string // the probe script's output
	ProbeExit    int
	Counters     string // the counters script's output
	CommandOut   string // a command's output
	CommandExit  int
	CommandBlock bool // a command blocks until it is killed
	TarExit      int  // the copy-in's tar
	TarBlock     bool // the copy-in's tar hangs (without reading) until it is killed
	CreateExit   int  // docker create's exit; not 0: refused, its name in use
	CreateBlock  bool // docker create hangs until it is killed
	CountersExit int  // the counters' exec exit; not 0: it failed (a fork bomb holds every slot, or it was killed)
	CommandGone  bool // the container ends during a command (its main process killed): exit 137, and gone after
	// The daemon's events for a command's exec come from files the command's fake writes: exec-<nonce> (its
	// exec_start action) and exit-<nonce> (the exit code the daemon logs).
	ClientFail          bool // the command runs and its exit is logged, then the client reports an error and exits 1
	ClientFailEarly     bool // the client exits 1, silently, before the exec starts: no event at all
	ClientExit          int  // not 0: the client exits with this, silently, whatever the command's logged exit
	EventsFail          bool // docker events fails at once
	EventsBare          bool // the events carry no exec IDs or exit codes (an old engine)
	EventsTwice         bool // every event comes twice (from the replay and live)
	EventsEndAfterStart bool // the events stream ends with an error right after a command's exec_start
	// Markers after start's own (the one before each command): MarkerExit, when not 0, is their client's exit, as when
	// a fork bomb left running holds every process slot; MarkerUnrecorded leaves them out of the events.
	MarkerExit       int
	MarkerUnrecorded bool
	RmFail           bool   // docker rm fails (the daemon stopped answering): the container stays
	PS               string // docker ps output
	Volumes          string // docker volume ls output
	Env              bool   // record the environment of each call too

	// Images and pulls (images.go). Images answers image inspect per reference (nil: Image answers every one);
	// AfterPull and AfterBuild add answers once a pull of that reference, or a build, has run.
	Images     map[string]string
	AfterPull  map[string]string
	AfterBuild map[string]string
	PullExit   int
	BuildExit  int
	BuildID    string // written to the build's --iidfile
	CheckOut   string // the built image's check (docker run)
	CheckExit  int
	ImageLS    string // docker image ls output
	ImageRmErr string // docker image rm fails with this message
	// Volumes (deps.go, cleanup.go). VolumeUntilCreate: volume inspect says "no such volume" until a volume create ran.
	VolumeUntilCreate bool
	VolumeCreated     string            // volume inspect --format {{.Name}}\t{{.CreatedAt}} output
	VolumeData        string            // volume inspect --format {{index .Labels "agentium.data"}} output
	VolumeUsers       string            // ps --filter volume=... output
	VolumeUsersBy     map[string]string // the same per volume, before VolumeUsers
	DF                string            // system df --format {{json .Volumes}} output
	IdleState         string            // container inspect --format {{.State.Status}}\t{{index .Config.Labels "agentium.data"}} output
	RmRefuse          string            // docker rm (without --force) fails with this message
	SeedExit          int               // a seed's tar (docker start --attach)
	PullStderr        string            // what a pull prints on stderr
	Seeders           string            // ps of a volume's seed containers: name, tab, run
}

func fakeDocker() int {
	dir := filepath.Dir(filepath.Dir(os.Args[0]))
	data, err := os.ReadFile(filepath.Join(dir, "scenario.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake docker:", err)
		return 99
	}
	var sc scenario
	if err := json.Unmarshal(data, &sc); err != nil {
		fmt.Fprintln(os.Stderr, "fake docker:", err)
		return 99
	}
	args := os.Args[1:]
	record := args
	if sc.Env {
		record = append(append([]string{}, args...), "ENV:"+strings.Join(os.Environ(), "|"))
	}
	line, _ := json.Marshal(record)
	calls := filepath.Join(dir, "calls")
	f, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 99
	}
	fmt.Fprintln(f, string(line))
	f.Close()
	if len(args) >= 4 && args[0] == "--config" && args[2] == "--host" {
		args = args[4:]
	}
	saveStdin := func() {
		n := countLines(calls)
		in, _ := io.ReadAll(os.Stdin)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("stdin-%d", n)), in, 0o600)
	}
	removed := func() bool {
		data, _ := os.ReadFile(calls)
		_, err := os.Stat(filepath.Join(dir, "gone"))
		return !sc.RmFail && strings.Contains(string(data), `"rm","--force"`) || err == nil
	}
	switch {
	case slices.Equal(args[:2], []string{"context", "inspect"}):
		if sc.ContextFail != "" {
			fmt.Fprintln(os.Stderr, sc.ContextFail)
			return 1
		}
		fmt.Printf("%q\n", sc.Endpoint)
	case args[0] == "version":
		if sc.Version == "" {
			fmt.Fprintln(os.Stderr, "Cannot connect to the Docker daemon at unix:///fake.sock. Is the docker daemon running?")
			return 1
		}
		fmt.Println(sc.Version)
	case args[0] == "info":
		fmt.Println(sc.Info)
	case slices.Equal(args[:2], []string{"image", "inspect"}):
		ref := args[len(args)-1]
		if answer, ok := imageAnswer(dir, sc, ref); ok {
			fmt.Println(answer)
			return 0
		}
		if sc.Images != nil || sc.AfterPull != nil || sc.AfterBuild != nil {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such image: %s\n", ref)
			return 1
		}
		if sc.Image == "" {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such image: %s\n", args[len(args)-1])
			return 1
		}
		fmt.Println(sc.Image)
	case slices.Equal(args[:2], []string{"volume", "inspect"}) && strings.Contains(strings.Join(args, " "), "CreatedAt"):
		fmt.Print(sc.VolumeCreated)
	case slices.Equal(args[:2], []string{"volume", "inspect"}) && strings.Contains(strings.Join(args, " "), "agentium.data"):
		fmt.Println(sc.VolumeData)
	case slices.Equal(args[:2], []string{"volume", "inspect"}):
		if _, err := os.Stat(filepath.Join(dir, "volume-created")); sc.VolumeUntilCreate && err != nil {
			fmt.Fprintf(os.Stderr, "Error response from daemon: get %s: no such volume\n", args[len(args)-1])
			return 1
		}
		if sc.Volume == "" {
			fmt.Fprintf(os.Stderr, "Error response from daemon: get %s: no such volume\n", args[len(args)-1])
			return 1
		}
		fmt.Println(sc.Volume)
	case args[0] == "create":
		if sc.CreateBlock {
			time.Sleep(time.Minute) // bounded, so a fake orphaned by a failing test does not linger
		}
		if sc.CreateExit != 0 {
			fmt.Fprintln(os.Stderr, `Error response from daemon: Conflict. The container name "/x" is already in use by container "y".`)
			return sc.CreateExit
		}
		fmt.Println(strings.Repeat("c", 64))
	case slices.Equal(args[:2], []string{"container", "inspect"}) && strings.Contains(strings.Join(args, " "), "agentium.data"):
		if sc.IdleState == "" {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", args[len(args)-1])
			return 1
		}
		fmt.Println(sc.IdleState)
	case slices.Equal(args[:2], []string{"container", "inspect"}):
		if sc.Inspect == "" || removed() {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", args[len(args)-1])
			return 1
		}
		if slices.Contains(args, "--format") {
			fmt.Println("running")
			return 0
		}
		fmt.Println(sc.Inspect)
	case args[0] == "cp":
		saveStdin()
	case args[0] == "start" && slices.Contains(args, "--attach"):
		saveStdin() // a seed's stream, to the volume's own tar
		return sc.SeedExit
	case args[0] == "start":
		fmt.Println(args[1])
	case args[0] == "exec":
		if removed() {
			fmt.Fprintln(os.Stderr, "Error response from daemon: No such container")
			return 1
		}
		switch script := args[len(args)-1]; {
		case slices.Contains(args, "tar"):
			if sc.TarBlock {
				time.Sleep(time.Minute) // bounded, so a fake orphaned by a failing test does not linger
			}
			saveStdin()
			return sc.TarExit
		case script == probeScript:
			fmt.Print(sc.Probe)
			return sc.ProbeExit
		case script == countersScript:
			if sc.CountersExit != 0 {
				fmt.Fprintln(os.Stderr, "OCI runtime exec failed: exec failed: unable to start container process: fork: resource temporarily unavailable")
				return sc.CountersExit
			}
			fmt.Print(sc.Counters)
		case len(args) >= 2 && args[len(args)-2] == ":":
			// markReady's marker: a no-op, recorded like any marked exec.
			data, _ := os.ReadFile(calls)
			later := strings.Count(string(data), `"sh","-c",":"`) > 1
			if later && sc.MarkerExit != 0 {
				fmt.Fprintln(os.Stderr, "OCI runtime exec failed: exec failed: unable to start container process: fork: resource temporarily unavailable")
				return sc.MarkerExit
			}
			if later && sc.MarkerUnrecorded {
				return 0
			}
			nonce := args[len(args)-1]
			sh := slices.Index(args, "sh")
			event(dir, "exec-"+nonce, "exec_start: "+strings.Join(args[sh:], " "))
			event(dir, "exit-"+nonce, "0")
			return 0
		case sc.ClientFailEarly:
			return 1
		default:
			nonce := args[len(args)-1]
			if !strings.HasPrefix(nonce, "agentium-exec-") {
				fmt.Fprintln(os.Stderr, "fake docker: a command without its exec marker", args)
				return 99
			}
			sh := slices.Index(args, "sh")
			event(dir, "exec-"+nonce, "exec_start: "+strings.Join(args[sh:], " "))
			switch {
			case sc.CommandGone:
				event(dir, "exit-"+nonce, "137")
				os.WriteFile(filepath.Join(dir, "gone"), nil, 0o600)
				return 137
			case sc.CommandBlock:
				time.Sleep(time.Minute) // bounded, so a fake orphaned by a failing test does not linger
				return 0
			}
			fmt.Print(sc.CommandOut)
			event(dir, "exit-"+nonce, strconv.Itoa(sc.CommandExit))
			switch {
			case sc.ClientFail:
				fmt.Fprintln(os.Stderr, "error during connect: Get \"http://%2Fvar%2Frun%2Fdocker.sock/v1.47/exec/x/json\": EOF")
				return 1
			case sc.ClientExit != 0:
				return sc.ClientExit
			}
			return sc.CommandExit
		}
	case args[0] == "rm" && sc.RmRefuse != "" && !slices.Contains(args, "--force"):
		fmt.Fprintln(os.Stderr, sc.RmRefuse)
		return 1
	case args[0] == "rm":
		if sc.RmFail {
			fmt.Fprintln(os.Stderr, "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
			return 1
		}
		fmt.Println(args[len(args)-1])
	case args[0] == "events":
		return fakeEvents(dir, sc)
	case args[0] == "ps" && strings.Contains(strings.Join(args, " "), "volume=") && strings.Contains(strings.Join(args, " "), "agentium.run"):
		fmt.Print(sc.Seeders)
	case args[0] == "ps" && strings.Contains(strings.Join(args, " "), "volume="):
		for name, users := range sc.VolumeUsersBy {
			if slices.Contains(args, "volume="+name) {
				fmt.Print(users)
				return 0
			}
		}
		fmt.Print(sc.VolumeUsers)
	case args[0] == "ps":
		fmt.Print(sc.PS)
	case args[0] == "pull":
		appendLine(filepath.Join(dir, "pulled"), args[len(args)-1])
		fmt.Fprint(os.Stderr, sc.PullStderr)
		fmt.Println("pulled", args[len(args)-1])
		return sc.PullExit
	case args[0] == "build":
		saveStdin()
		if sc.BuildExit != 0 {
			fmt.Fprintln(os.Stderr, "E: Unable to locate package less")
			return sc.BuildExit
		}
		if i := slices.Index(args, "--iidfile"); i >= 0 && sc.BuildID != "" {
			os.WriteFile(args[i+1], []byte(sc.BuildID), 0o600)
		}
		os.WriteFile(filepath.Join(dir, "built"), nil, 0o600)
		fmt.Println("Successfully built")
	case args[0] == "run":
		fmt.Print(sc.CheckOut)
		return sc.CheckExit
	case slices.Equal(args[:2], []string{"image", "ls"}):
		fmt.Print(sc.ImageLS)
	case slices.Equal(args[:2], []string{"image", "rm"}):
		if sc.ImageRmErr != "" {
			fmt.Fprintln(os.Stderr, sc.ImageRmErr)
			return 1
		}
		fmt.Println("Untagged:", args[2])
	case slices.Equal(args[:2], []string{"volume", "create"}):
		os.WriteFile(filepath.Join(dir, "volume-created"), nil, 0o600)
		fmt.Println(args[len(args)-1])
	case slices.Equal(args[:2], []string{"system", "df"}):
		fmt.Println(sc.DF)
	case slices.Equal(args[:2], []string{"volume", "ls"}):
		fmt.Print(sc.Volumes)
	case slices.Equal(args[:2], []string{"volume", "rm"}):
		fmt.Println(args[2])
	default:
		fmt.Fprintln(os.Stderr, "fake docker: unexpected call", args)
		return 99
	}
	return 0
}

// imageAnswer is image inspect's answer for ref: from Images, or from AfterPull or AfterBuild once that ran.
func imageAnswer(dir string, sc scenario, ref string) (string, bool) {
	if answer, ok := sc.Images[ref]; ok {
		return answer, true
	}
	if pulled, _ := os.ReadFile(filepath.Join(dir, "pulled")); slices.Contains(strings.Split(string(pulled), "\n"), ref) {
		if answer, ok := sc.AfterPull[ref]; ok {
			return answer, true
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "built")); err == nil {
		if answer, ok := sc.AfterBuild[ref]; ok {
			return answer, true
		}
	}
	return "", false
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintln(f, line)
	f.Close()
}

// event writes one of the daemon's records for the fake events stream, whole (a rename), so it is never read half
// written.
func event(dir, name, body string) {
	tmp := filepath.Join(dir, "tmp-"+name)
	os.WriteFile(tmp, []byte(body), 0o600)
	os.Rename(tmp, filepath.Join(dir, name))
}

// fakeEvents streams the daemon's events for the container from the records the command fakes write, in the shape of
// docker events --format '{{json .}}': exec_start, then exec_die, for each command, and die once the container is
// gone. It runs until it is killed (bounded, so a fake orphaned by a failing test does not linger).
func fakeEvents(dir string, sc scenario) int {
	if sc.EventsFail {
		fmt.Fprintln(os.Stderr, "Error response from daemon: events: simulated failure")
		return 1
	}
	const id = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" // testdata/inspect.json's
	emit := func(action string, attrs map[string]string) {
		if sc.EventsBare {
			attrs = map[string]string{}
		}
		line, _ := json.Marshal(map[string]any{"Type": "container", "Action": action, "Actor": map[string]any{"ID": id, "Attributes": attrs}})
		fmt.Println(string(line))
		if sc.EventsTwice {
			fmt.Println(string(line))
		}
	}
	seen := map[string]bool{}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			name := e.Name()
			if seen[name] {
				continue
			}
			body, _ := os.ReadFile(filepath.Join(dir, name))
			switch kind, nonce, _ := strings.Cut(name, "-"); {
			case kind == "exec" && strings.HasPrefix(nonce, "agentium-exec-"):
				emit(string(body), map[string]string{"execID": "id-" + nonce})
				if sc.EventsEndAfterStart && strings.Contains(string(body), "exec sh -c") {
					fmt.Fprintln(os.Stderr, "Error response from daemon: unexpected EOF")
					return 1
				}
			case kind == "exit" && strings.HasPrefix(nonce, "agentium-exec-"):
				emit("exec_die", map[string]string{"execID": "id-" + nonce, "exitCode": string(body)})
			case name == "gone":
				emit("die", map[string]string{"exitCode": "137"})
			default:
				continue
			}
			seen[name] = true
		}
	}
	return 0
}

func countLines(path string) int {
	data, _ := os.ReadFile(path)
	return strings.Count(string(data), "\n")
}
