//go:build !darwin && !linux

package runner

import "errors"

// canWaitWithoutReaping: whether this system's waitExit works; without it Run refuses to start a command.
const canWaitWithoutReaping = false

// waitExit cannot wait without reaping here.
func waitExit(int) error {
	return errors.New("waiting for an exit without reaping is not supported here")
}
