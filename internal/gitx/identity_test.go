package gitx

import (
	"context"
	"strings"
	"testing"
)

// Identity names the program, its version and the locale, and changes when any locale variable does; the variables
// it reads reach git, since Environ keeps them.
func TestIdentityChangesWithTheLocale(t *testing.T) {
	ctx := context.Background()
	for _, name := range localeVariables {
		t.Setenv(name, "")
	}
	plain, err := Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain, "git version ") || !strings.Contains(plain, "/git\n") || !strings.Contains(plain, "LC_ALL=\nLC_CTYPE=\nLANG=") {
		t.Errorf("identity = %q, want the program's path, its version and the locale variables", plain)
	}
	if again, err := Identity(ctx); err != nil || again != plain {
		t.Errorf("asked again: %q, %v; want the same", again, err)
	}
	seen := map[string]string{plain: "no locale"}
	for _, name := range localeVariables {
		t.Setenv(name, "tr_TR.UTF-8")
		got, err := Identity(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if earlier, ok := seen[got]; ok {
			t.Errorf("%s=tr_TR.UTF-8 has the identity of %s", name, earlier)
		}
		seen[got] = name
		t.Setenv(name, "")
		if kept := strings.Join(Environ([]string{name + "=tr_TR.UTF-8"}), " "); !strings.Contains(kept, name+"=tr_TR.UTF-8") {
			t.Errorf("Environ drops %s, which Identity reads: %q", name, kept)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Identity(cancelled); err == nil {
		t.Error("a cancelled context got an identity")
	}
}
