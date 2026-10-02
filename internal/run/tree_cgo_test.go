//go:build cgo && (darwin || linux)

package run

import (
	"errors"
	"io/fs"
	"testing"
)

// A removal error names its path below the root, which the grade chooses: a deep one keeps its first and last two
// names, and every name is quoted, so no newline or terminal escape reaches the console.
func TestBelowErrorShortensAndQuotesItsPath(t *testing.T) {
	t.Parallel()
	deep := &belowError{path: []string{"cache", "a", "b\nc", "d", "e\x1b[2J"}, err: fs.ErrPermission}
	if got, want := deep.Error(), `"cache"/…/"d"/"e\x1b[2J": permission denied`; got != want {
		t.Errorf("deep: %q, want %q", got, want)
	}
	if !errors.Is(deep, fs.ErrPermission) {
		t.Error("the cause is lost")
	}
	short := &belowError{path: []string{"cache", "x\ny"}, err: fs.ErrPermission}
	if got, want := short.Error(), `"cache"/"x\ny": permission denied`; got != want {
		t.Errorf("short: %q, want %q", got, want)
	}
}
