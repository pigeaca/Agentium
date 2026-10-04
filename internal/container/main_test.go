package container

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// scenario is what the fake docker answers, read from $DOCKER_CONFIG/scenario.json (DOCKER_CONFIG is on the client's
// allowlist, so the driver passes it through unchanged). Every call is appended to $DOCKER_CONFIG/calls as one JSON
// array of its argv after the binary, and what it reads from stdin is kept in $DOCKER_CONFIG/stdin-<n>.
type scenario struct {
	Endpoint     string // the context's endpoint
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
	CommandBlock bool   // a command blocks until it is killed
	TarExit      int    // the copy-in's tar
	CreateExit   int    // docker create's exit; not 0: refused, its name in use
	PS           string // docker ps output
	Volumes      string // docker volume ls output
	Env          bool   // record the environment of each call too
}

func fakeDocker() int {
	dir := os.Getenv("DOCKER_CONFIG")
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
	if len(args) >= 2 && args[0] == "--host" {
		args = args[2:]
	}
	saveStdin := func() {
		n := countLines(calls)
		in, _ := io.ReadAll(os.Stdin)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("stdin-%d", n)), in, 0o600)
	}
	removed := func() bool {
		data, _ := os.ReadFile(calls)
		return strings.Contains(string(data), `"rm","--force"`)
	}
	switch {
	case slices.Equal(args[:2], []string{"context", "inspect"}):
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
		if sc.Image == "" {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such image: %s\n", args[len(args)-1])
			return 1
		}
		fmt.Println(sc.Image)
	case slices.Equal(args[:2], []string{"volume", "inspect"}):
		if sc.Volume == "" {
			fmt.Fprintf(os.Stderr, "Error response from daemon: get %s: no such volume\n", args[len(args)-1])
			return 1
		}
		fmt.Println(sc.Volume)
	case args[0] == "create":
		if sc.CreateExit != 0 {
			fmt.Fprintln(os.Stderr, `Error response from daemon: Conflict. The container name "/x" is already in use by container "y".`)
			return sc.CreateExit
		}
		fmt.Println(strings.Repeat("c", 64))
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
	case args[0] == "start":
		fmt.Println(args[1])
	case args[0] == "exec":
		if removed() {
			fmt.Fprintln(os.Stderr, "Error response from daemon: No such container")
			return 1
		}
		switch script := args[len(args)-1]; {
		case slices.Contains(args, "tar"):
			saveStdin()
			return sc.TarExit
		case script == probeScript:
			fmt.Print(sc.Probe)
			return sc.ProbeExit
		case script == countersScript:
			fmt.Print(sc.Counters)
		case sc.CommandBlock:
			time.Sleep(time.Minute) // bounded, so a fake orphaned by a failing test does not linger
		default:
			fmt.Print(sc.CommandOut)
			return sc.CommandExit
		}
	case args[0] == "rm":
		fmt.Println(args[len(args)-1])
	case args[0] == "ps":
		fmt.Print(sc.PS)
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

func countLines(path string) int {
	data, _ := os.ReadFile(path)
	return strings.Count(string(data), "\n")
}
