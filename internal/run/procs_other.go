//go:build !(darwin && cgo)

package run

import (
	"context"
	"time"
)

// processesUnder finds nothing outside macOS: the grading sandbox is macOS's, and grades elsewhere (none yet) would
// need their own sweep (Linux: /proc/<pid>/cwd, exe and fd).
func processesUnder([]string) ([]int, error) { return nil, nil }

// stopProcessesUnder stops nothing outside macOS (see processesUnder).
func stopProcessesUnder([]string) ([]string, error) { return nil, nil }

// stopSandboxed stops nothing outside macOS (see processesUnder).
func stopSandboxed(string, string) ([]string, error) { return nil, nil }

// usingGrade finds nothing outside macOS (see processesUnder).
func usingGrade(string, string, []string) ([]string, error) { return nil, nil }

// snapshot sees nothing outside macOS (see processesUnder).
func (d *descendants) snapshot() error { return nil }

// observe watches nothing outside macOS (see processesUnder).
func (d *descendants) observe(context.Context, int) {}

// stopDescendants stops nothing outside macOS (see processesUnder).
func stopDescendants(*descendants, time.Time, func([]int) bool) ([]string, error) { return nil, nil }

// reportCodex finds nothing outside macOS (see processesUnder).
func reportCodex(codexSweep) ([]leftProcess, error) { return nil, nil }

// codexReportPIDs finds nothing outside macOS (see processesUnder).
func codexReportPIDs(codexSweep) ([]int, error) { return nil, nil }

// identityNow finds no process outside macOS (see processesUnder).
func identityNow(int) (identity, bool) { return identity{}, false }
