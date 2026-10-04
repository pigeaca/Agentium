package claude

import (
	"slices"
	"strings"
	"testing"
)

// An agent started in a module's folder (Dir inside Repo) keeps its whole checkout: Repo is a working folder (--add-dir),
// the sandbox lets it write there besides its build cache, and the build tools' environment names the checkout (a
// Python module's import root is relative to it). Without Repo, or with Repo the same as Dir, the command is the
// root's, byte for byte; a Dir outside Repo is refused.
func TestCommandOfAnAgentStartedInAModule(t *testing.T) {
	root := toolInvocation(t, "python")
	root.ImportRoot = "svc/src"
	rootArgs, rootEnv, err := root.Command(parentEnv)
	if err != nil {
		t.Fatal(err)
	}
	same := root
	same.Repo = root.Dir
	if args, env, err := same.Command(parentEnv); err != nil || !slices.Equal(args, rootArgs) || !slices.Equal(env, rootEnv) {
		t.Errorf("Repo == Dir changed the command: %v", err)
	}
	if slices.Contains(rootArgs, "--add-dir") {
		t.Errorf("a root run has another working folder: %v", rootArgs)
	}

	inModule := root
	inModule.Repo, inModule.Dir = root.Dir, root.Dir+"/svc"
	args, _, err := inModule.Command(parentEnv)
	if err != nil {
		t.Fatal(err)
	}
	if flagValue(args, "--add-dir") != root.Dir {
		t.Errorf("--add-dir %q, want the checkout %s", flagValue(args, "--add-dir"), root.Dir)
	}
	env, settings := toolCommand(t, inModule, parentEnv)
	if !strings.HasPrefix(env["PYTHONPATH"], root.Dir+"/svc/src") {
		t.Errorf("PYTHONPATH %q: the import root is relative to the checkout", env["PYTHONPATH"])
	}
	_, fs := sandboxOf(settings)
	writable, _ := fs["allowWrite"].([]any)
	if !slices.Contains(writable, any(root.Dir)) || !slices.Contains(writable, any(root.BuildCache)) {
		t.Errorf("allowWrite %v: want the checkout and the build cache", writable)
	}

	for _, dir := range []string{"/work/runs/r1", "/elsewhere", "/work/runs/r1/repo-other"} {
		outside := root
		outside.Repo, outside.Dir = root.Dir, dir
		if _, _, err := outside.Command(parentEnv); err == nil || !strings.Contains(err.Error(), "is not inside its checkout") {
			t.Errorf("Dir %s outside Repo: %v", dir, err)
		}
	}
}
