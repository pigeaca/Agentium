//go:build !(darwin && cgo)

package run

// processesUnder finds nothing outside macOS: the grading sandbox is macOS's, and grades elsewhere (none yet) would
// need their own sweep (Linux: /proc/<pid>/cwd, exe and fd).
func processesUnder([]string) ([]int, error) { return nil, nil }

// stopProcessesUnder stops nothing outside macOS (see processesUnder).
func stopProcessesUnder([]string) ([]string, error) { return nil, nil }

// stopSandboxed stops nothing outside macOS (see processesUnder).
func stopSandboxed(string, string) ([]string, error) { return nil, nil }

// usingGrade finds nothing outside macOS (see processesUnder).
func usingGrade(string, string, []string) ([]string, error) { return nil, nil }
