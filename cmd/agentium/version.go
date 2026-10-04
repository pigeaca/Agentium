package main

import (
	"regexp"
	"runtime/debug"
	"strings"
)

// pseudoVersion matches the pseudo-versions Go stamps on a build that is not exactly at a tag: after no tag
// (v0.0.0-20261004094733-d9fc500633ab), after a release (v0.1.1-0.20261004094733-d9fc500633ab), after a prerelease
// (v1.2.3-pre.0.20261004094733-d9fc500633ab), optionally with +dirty. They name a commit, not a release.
var pseudoVersion = regexp.MustCompile(`(^|[-.])\d{14}-[0-9A-Za-z]+(\+dirty)?$`)

// released reports whether v is a real tag: set, not "(devel)", not a pseudo-version, not dirty.
func released(v string) bool {
	return v != "" && v != "(devel)" && !pseudoVersion.MatchString(v) && !strings.HasSuffix(v, "+dirty")
}

// buildVersion is the version `agentium version` shows. A release build's -ldflags value wins; otherwise a real tag
// the Go toolchain recorded (`go install github.com/pigeaca/agentium/cmd/agentium@v0.1.0` sets it); otherwise, for a
// build from a checkout (it has a VCS revision), "dev" plus the short revision as build metadata and ".modified" when
// the tree had uncommitted changes; otherwise (`go install ...@main` or @<commit> have no VCS data) the pseudo-version
// itself, which names the commit.
func buildVersion(linked string, info *debug.BuildInfo, ok bool) string {
	if linked != "" && linked != "dev" {
		return linked
	}
	if !ok || info == nil {
		return "dev"
	}
	v := info.Main.Version
	if released(v) {
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
		if v != "" && v != "(devel)" {
			return v
		}
		return "dev"
	}
	detail := revision[:min(len(revision), 12)]
	if modified {
		detail += ".modified"
	}
	return "dev+" + detail
}
