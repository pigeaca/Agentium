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
		"java -version":    "openjdk version \"21.0.4\" 2024-07-16\nOpenJDK Runtime Environment\n",
		"mvn --version":    "Apache Maven 3.9.9 (8e8579a9e76f7d015ee5ec7bfcdc97d260186937)\nMaven home: /opt/maven\n",
		"rustc --version":  strings.Repeat("é", 150) + "\n", // 300 bytes: cut to 200, on a rune boundary
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
	if r := got["rustc"]; len(r) != 200 || r != strings.Repeat("é", 100) {
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

// HostVersions runs commands as Agentium's own: stdout and stderr together, no credentials, no Go toolchain download,
// and a failure is an error.
func TestHostVersions(t *testing.T) {
	run := HostVersions(t.TempDir(), []string{"PATH=/usr/bin:/bin", "GITHUB_TOKEN=secret", "GOTOOLCHAIN=auto"})
	ctx := context.Background()
	out, err := run(ctx, []string{"sh", "-c", `echo "out $GOTOOLCHAIN ${GITHUB_TOKEN:-none}"; echo err >&2`})
	if err != nil || out != "out local none\nerr\n" {
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
