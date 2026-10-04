package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) {
	settings := func(revision, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: modified}}
	}
	tests := []struct {
		name   string
		linked string
		info   *debug.BuildInfo
		ok     bool
		want   string
	}{
		{"linked release", "v1.2.3", &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}, true, "v1.2.3"},
		{"go install", "dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, true, "v0.1.0"},
		{"local build, clean", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings("0123456789abcdef", "false")}, true, "dev (0123456789ab)"},
		{"local build, modified", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings("0123456789abcdef", "true")}, true, "dev (0123456789ab, modified)"},
		{"no vcs information", "dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true, "dev"},
		{"no build info", "dev", nil, false, "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildVersion(tt.linked, tt.info, tt.ok); got != tt.want {
				t.Errorf("buildVersion = %q, want %q", got, tt.want)
			}
		})
	}
}
