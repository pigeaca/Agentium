package project

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/buildtool"
	"github.com/pigeaca/agentium/internal/gitx"
)

// Module listing limits: how deep below the root a build file is looked for, and how many modules init shows.
const (
	MaxModuleDepth = 4
	MaxModules     = 20
)

// noTestCommands is the warning for a project whose detected folder holds no test command.
const noTestCommands = "No test command detected: tasks will need explicit verification commands."

// Module is a candidate module of a monorepo: a folder holding a build file.
type Module struct {
	Path  string   `json:"path"`  // slash-separated, relative to the repository root
	Tools []string `json:"tools"` // the build tools its files name (buildtool profile names)
}

// FindModules lists the folders below root, up to MaxModuleDepth levels deep, that hold a build file. A module's own
// subfolders are not looked into: nested modules are folded into their nearest parent. Hidden folders and the usual
// dependency and output folders are skipped. It returns at most MaxModules, in path order, and how many there are in
// all. root itself is never listed: a root with a build file is a project as it is.
func FindModules(root string) (modules []Module, total int) {
	var walk func(rel string, depth int)
	walk = func(rel string, depth int) {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return
		}
		for _, e := range entries { // sorted by name
			if !e.IsDir() || skipFolder(e.Name()) {
				continue
			}
			child := path.Join(rel, e.Name())
			if tools := buildtool.DetectIn(filepath.Join(root, filepath.FromSlash(child))); len(tools) > 0 {
				total++
				if len(modules) < MaxModules {
					modules = append(modules, Module{Path: child, Tools: tools})
				}
				continue
			}
			if depth < MaxModuleDepth {
				walk(child, depth+1)
			}
		}
	}
	walk("", 1)
	return modules, total
}

// skipFolder reports whether a folder of that name never holds a module: hidden ones, and what tools fetch or build.
func skipFolder(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	return slices.Contains([]string{"node_modules", "vendor", "target", "build", "dist", "venv", "__pycache__", "testdata"}, name)
}

// ErrModule means a module path cannot be used; its message says why.
var ErrModule = errors.New("invalid module")

// ValidateModule checks a --module value against the repository at root and returns it in its stored form (slash-
// separated, clean, relative). The path must name a folder inside the repository, reached through no link, that holds a
// build file Agentium detects, and that HEAD commits as a folder under exactly that spelling (runs check out commits,
// so an untracked or ignored folder, or one only a case-insensitive file system finds under another spelling, is no
// module). "" is valid and means no module.
func ValidateModule(ctx context.Context, root, module string) (string, error) {
	if strings.TrimSpace(module) == "" {
		return "", nil
	}
	clean := path.Clean(filepath.ToSlash(module))
	switch {
	case filepath.IsAbs(module) || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("%w %q: it must be a folder inside the repository, written relative to its root", ErrModule, module)
	case clean == ".":
		return "", fmt.Errorf("%w %q: that is the repository's root; --module '' selects it", ErrModule, module)
	}
	dir := root
	for _, part := range strings.Split(clean, "/") {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("%w %q: there is no such folder in the repository", ErrModule, module)
		case err != nil:
			return "", fmt.Errorf("%w %q: %v", ErrModule, module, err)
		case info.Mode()&fs.ModeSymlink != 0:
			return "", fmt.Errorf("%w %q: %s is a link, and a module must be a real folder", ErrModule, module, part)
		case !info.IsDir():
			return "", fmt.Errorf("%w %q: %s is not a folder", ErrModule, module, part)
		}
	}
	if len(buildtool.DetectIn(dir)) == 0 {
		return "", fmt.Errorf("%w %q: no build file Agentium knows (go.mod, pom.xml, pyproject.toml, ...) is in it", ErrModule, module)
	}
	// Runs and validations check out commits: the module must be a folder of HEAD (a tree), spelled as the tree has it.
	out, err := gitx.Output(ctx, nil, "-C", root, "--literal-pathspecs", "ls-tree", "-z", "HEAD", "--", clean)
	if err != nil {
		return "", fmt.Errorf("%w %q: %v", ErrModule, module, err)
	}
	committed := false
	for _, entry := range strings.Split(string(out), "\x00") {
		if meta, name, ok := strings.Cut(entry, "\t"); ok && name == clean && strings.HasPrefix(meta, "040000 tree ") {
			committed = true
		}
	}
	if !committed {
		return "", fmt.Errorf("%w %q: HEAD has no such folder (is it untracked, ignored, or spelled with other letter case?)", ErrModule, module)
	}
	return clean, nil
}

// CommandsIn is the test commands of the module's folder ("": the root's), read from the working tree now.
func (i Info) CommandsIn(module string) []string {
	return testCommands(filepath.Join(i.Root, filepath.FromSlash(module)))
}

// WithModule is the discovery of the module's folder instead of the root's: the test commands are the module's, and
// the root's warning about missing ones follows them. module must be valid (ValidateModule); "" returns info as it is.
func (i Info) WithModule(module string) Info {
	i.Module = module
	if module == "" {
		return i
	}
	i.TestCommands = testCommands(filepath.Join(i.Root, filepath.FromSlash(module)))
	i.Warnings = slices.DeleteFunc(slices.Clone(i.Warnings), func(w string) bool { return w == noTestCommands })
	if len(i.TestCommands) == 0 {
		i.TestCommands = []string{}
		i.Warnings = append(i.Warnings, noTestCommands)
	}
	return i
}
