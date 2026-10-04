package main

import (
	"regexp"
	"runtime/debug"
	"strings"
)

// pseudoVersion matches the pseudo-version Go stamps on a build from a checkout that is not exactly at a tag
// (v0.0.0-20261004094733-d9fc500633ab, optionally with +dirty); it names a commit, not a release.
var pseudoVersion = regexp.MustCompile(`-\d{14}-[0-9a-f]{12}(\+dirty)?$`)

// released reports whether v is a real tag: set, not "(devel)", not a pseudo-version, not dirty.
func released(v string) bool {
	return v != "" && v != "(devel)" && !pseudoVersion.MatchString(v) && !strings.HasSuffix(v, "+dirty")
}

// buildVersion is the version `agentium version` shows. A release build's -ldflags value wins; otherwise the module
// version the Go toolchain recorded (`go install github.com/pigeaca/agentium/cmd/agentium@v0.1.0` sets it); otherwise
// a local build: "dev" plus the short VCS revision as build metadata and ".modified" when the tree had uncommitted changes.
func buildVersion(linked string, info *debug.BuildInfo, ok bool) string {
	if linked != "" && linked != "dev" {
		return linked
	}
	if !ok || info == nil {
		return "dev"
	}
	if v := info.Main.Version; released(v) {
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
		detail += ".modified"
	}
	return "dev+" + detail
}
