package cli

import (
	"bytes"
	"flag"
	"slices"
	"testing"
)

func TestParseArgsInterspersedAndTerminator(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := Env{Stdout: &stdout, Stderr: &stderr}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	patch := fs.Bool("patch", false, "")
	got, code, ok := parseArgs(env, fs, []string{"a", "--patch", "b", "--", "-c", "--patch"}, "usage\n")
	if !ok || code != ExitOK || !*patch || !slices.Equal(got, []string{"a", "b", "-c", "--patch"}) {
		t.Errorf("got %q, code %d, ok %v, patch %v", got, code, ok, *patch)
	}
}
