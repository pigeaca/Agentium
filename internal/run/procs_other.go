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

// codexLeftoverPIDs finds nothing outside macOS (see processesUnder).
func codexLeftoverPIDs(codexSweep) (kill, report []int, err error) { return nil, nil, nil }

// stopCodexLeftovers stops nothing outside macOS (see processesUnder).
func stopCodexLeftovers(codexSweep, func([]int) bool) ([]string, []leftProcess, error) {
	return nil, nil, nil
}

// leftAlive finds no process outside macOS (see processesUnder).
func leftAlive(leftProcess) bool { return false }

// stopLeft stops nothing outside macOS (see processesUnder).
func stopLeft(leftProcess) error { return nil }
