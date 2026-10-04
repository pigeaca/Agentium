package main

import (
	"runtime/debug"
)

// buildVersion is the version `agentium version` shows. A release build's -ldflags value wins; otherwise the module
// version the Go toolchain recorded (`go install github.com/pigeaca/agentium/cmd/agentium@v0.1.0` sets it); otherwise
// a local build: "dev" with the short VCS revision and ", modified" when the tree had uncommitted changes.
func buildVersion(linked string, info *debug.BuildInfo, ok bool) string {
	if linked != "" && linked != "dev" {
		return linked
	}
	if !ok || info == nil {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	detail := revision[:min(len(revision), 12)]
	if modified {
		detail += ", modified"
	}
	return "dev (" + detail + ")"
}
