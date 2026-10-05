package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDetectToolchainAsksEachToolOnce(t *testing.T) {
	var asked []string
	outputs := map[string]string{
		"go env GOVERSION": "\ngo1.27.1\n",
		// Lines before the version (JAVA_TOOL_OPTIONS, Maven's warnings) are not taken for it.
		"java -version": "Picked up JAVA_TOOL_OPTIONS: -Dfile.encoding=UTF-8 -Dversion=1.2\nopenjdk version \"21.0.4\" 2024-07-16\nOpenJDK Runtime Environment\n",
		"mvn --version": "WARNING: A terminally deprecated method in sun.misc.Unsafe has been called\nWARNING: version 3.1 of x\n" +
			"Apache Maven 3.9.9 (8e8579a9e76f7d015ee5ec7bfcdc97d260186937)\nMaven home: /opt/maven\n",
		"cargo --version": "error: no such command\n",                    // no version line: left out
		"rustc --version": "rustc 1.0" + strings.Repeat("é", 150) + "\n", // 309 bytes: cut to 200, mid-rune
	}
	run := func(_ context.Context, args []string) (string, error) {
		key := strings.Join(args, " ")
		asked = append(asked, key)
		if out, ok := outputs[key]; ok {
			return out, nil
		}
		return "", errors.New("not found")
	}
	got, err := DetectToolchain(context.Background(), []string{"go", "maven", "gradle", "cargo", "unknown"}, run)
	if err != nil {
		t.Fatal(err)
	}
	if got["go"] != "go1.27.1" || got["java"] != `openjdk version "21.0.4" 2024-07-16` || !strings.HasPrefix(got["maven"], "Apache Maven 3.9.9") {
		t.Errorf("toolchain = %q", got)
	}
	if _, ok := got["cargo"]; ok {
		t.Errorf("a tool that failed is recorded: %q", got)
	}
	if r := got["rustc"]; len(r) != 199 || r != "rustc 1.0"+strings.Repeat("é", 95) { // the cut rune is dropped
		t.Errorf("a long version: %d bytes", len(r))
	}
	if want := "go env GOVERSION,mvn --version,java -version,cargo --version,rustc --version"; strings.Join(asked, ",") != want {
		t.Errorf("asked %s, want %s (java once for Maven and Gradle)", strings.Join(asked, ","), want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DetectToolchain(ctx, []string{"go"}, func(context.Context, []string) (string, error) { return "", ctx.Err() }); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// Python's version is the interpreter uv finds first (warmPython's), and python3 on PATH only without uv.
func TestDetectToolchainPython(t *testing.T) {
	for _, tt := range []struct {
		name    string
		outputs map[string]string
		want    string
		asked   string
	}{
		{"uv", map[string]string{"uv python find --system --no-project --show-version": "3.12.13\n", "python3 --version": "Python 3.9.6\n"}, "3.12.13",
			"uv python find --system --no-project --show-version"},
		{"no uv", map[string]string{"python3 --version": "Python 3.9.6\n"}, "Python 3.9.6",
			"uv python find --system --no-project --show-version,python3 --version"},
		{"neither", map[string]string{}, "", "uv python find --system --no-project --show-version,python3 --version"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var asked []string
			got, err := DetectToolchain(context.Background(), []string{"python"}, func(_ context.Context, args []string) (string, error) {
				key := strings.Join(args, " ")
				asked = append(asked, key)
				if out, ok := tt.outputs[key]; ok {
					return out, nil
				}
				return "", errors.New("not found")
			})
			if err != nil || got["python"] != tt.want || strings.Join(asked, ",") != tt.asked {
				t.Errorf("python %q (asked %v), %v; want %q", got["python"], asked, err, tt.want)
			}
		})
	}
}

// HostVersions runs commands as Agentium's own: stdout and stderr together, no credentials, no Go toolchain download,
// and a failure is an error.
func TestHostVersions(t *testing.T) {
	run := HostVersions(t.TempDir(), []string{"PATH=/usr/bin:/bin", "GITHUB_TOKEN=secret", "GOTOOLCHAIN=auto"})
	ctx := context.Background()
	out, err := run(ctx, []string{"sh", "-c", `echo "out $GOTOOLCHAIN $RUSTUP_AUTO_INSTALL $UV_PYTHON_DOWNLOADS ${GITHUB_TOKEN:-none}"; echo err >&2`})
	if err != nil || out != "out local 0 never none\nerr\n" {
		t.Errorf("output %q, %v", out, err)
	}
	if _, err := run(ctx, []string{"sh", "-c", "exit 3"}); err == nil {
		t.Error("a failing command must be an error")
	}
	if _, err := run(ctx, []string{"no-such-tool-agentium"}); err == nil {
		t.Error("a missing tool must be an error")
	}
	big, err := run(ctx, []string{"sh", "-c", "head -c 200000 /dev/zero | tr '\\0' x"})
	if err != nil || len(big) != 64<<10 {
		t.Errorf("a long output keeps %d bytes, %v", len(big), err)
	}
}
