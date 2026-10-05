package codex

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// A trusted checkout loads its .codex/config.toml (the plan's decision 7, re-decided 2026-10-04), and Codex 0.160.0
// merges config layers deeply: the run's -c overrides (session flags) win key by key over the project's layer, but a
// project key the overrides do not set stays, and inside a table they set the project's other entries stay too. The
// app server's config/read, given the run's exact overrides and a hostile project config, showed it (the plan's
// precedence notes): the project kept sandbox_mode = "danger-full-access", mcp_servers, sandbox_workspace_write, a
// feature (network_proxy), shell_environment_policy.include_only and set entries, and its own entries in the agentium
// profile (a writable path, network.allow_local_binding), while every key the overrides set was the run's.
//
// So a checkout's Codex configuration is read before the run, and refused unless it holds only top-level, one-line
// settings of the keys in allowedProjectKeys: the model and its effort (which the run's own -m and -c override), and
// how the model writes and how much AGENTS.md loads (the project's own setup, as in the user's sessions). Anything
// else in .codex/ but its skills is refused too: rules (also --ignore-rules), hooks, agent roles, and whatever Codex may
// read there that Agentium has not checked.

// allowedProjectKeys are the settings a project's .codex/config.toml may hold.
var allowedProjectKeys = map[string]bool{"model": true, "model_reasoning_effort": true, "model_reasoning_summary": true,
	"model_verbosity": true, "project_doc_max_bytes": true}

// allowedLine is one allowed setting: a bare key, then a one-line string, number or boolean, then an optional comment.
var allowedLine = regexp.MustCompile(`^([A-Za-z0-9_]+)\s*=\s*("[^"\\]*"|'[^']*'|[+-]?[0-9][0-9_]*|true|false)\s*(#.*)?$`)

// ProjectConfigRefusal refuses a checkout (repo, started in module: "" for its root) whose Codex configuration could
// change what the run's settings protect: a .codex folder in the root or a folder down to the start folder (each a
// project layer Codex loads) that holds anything but config.toml and skills, or a config.toml with a setting outside
// allowedProjectKeys, a table, or anything Agentium cannot read as such a setting. Links are refused, never followed.
func ProjectConfigRefusal(repo, module string) error {
	dirs := []string{""}
	if module != "" {
		parts := strings.Split(path.Clean(module), "/")
		for i := range parts {
			dirs = append(dirs, path.Join(parts[:i+1]...))
		}
	}
	for _, dir := range dirs {
		rel := path.Join(dir, ".codex")
		folder := filepath.Join(repo, filepath.FromSlash(rel))
		info, err := os.Lstat(folder)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("the checkout's %s: %w", rel, err)
		}
		if !info.IsDir() {
			return refusal(rel, "is not a folder")
		}
		entries, err := os.ReadDir(folder)
		if err != nil {
			return fmt.Errorf("the checkout's %s: %w", rel, err)
		}
		for _, e := range entries {
			switch {
			case e.Name() == "skills" && e.Type().IsDir():
			case e.Name() == "config.toml" && e.Type().IsRegular():
				data, err := os.ReadFile(filepath.Join(folder, "config.toml"))
				if err != nil {
					return fmt.Errorf("the checkout's %s/config.toml: %w", rel, err)
				}
				if problem := checkProjectConfig(string(data)); problem != "" {
					return refusal(rel+"/config.toml", problem)
				}
			default:
				return refusal(path.Join(rel, e.Name()), "is not a file Agentium lets a Codex run load (only config.toml and skills/)")
			}
		}
	}
	return nil
}

// checkProjectConfig says what in a project's config.toml is not allowed, or "" when nothing is.
func checkProjectConfig(data string) string {
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := allowedLine.FindStringSubmatch(line)
		switch {
		case m == nil:
			return fmt.Sprintf("line %d (%.60s) is not a one-line setting of %s", i+1, line, allowedList())
		case !allowedProjectKeys[m[1]]:
			return fmt.Sprintf("line %d sets %s, which Agentium's settings must decide", i+1, m[1])
		}
	}
	return ""
}

func allowedList() string {
	return "model, model_reasoning_effort, model_reasoning_summary, model_verbosity or project_doc_max_bytes"
}

// refusal is ProjectConfigRefusal's error for what (a path in the checkout).
func refusal(what, why string) error {
	return fmt.Errorf("the checkout's %s %s: Codex merges a trusted project's configuration into the run's own, so a run could not be sure of its sandbox, permissions, network or environment. "+
		"A Codex run may load only .codex/skills and a .codex/config.toml setting %s; change the task's base or the arm's snapshot", what, why, allowedList())
}
