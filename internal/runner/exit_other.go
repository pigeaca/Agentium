//go:build !darwin && !linux

package runner

import "errors"

// waitExit cannot wait without reaping here: Run reaps first, then stops the group's rest.
func waitExit(int) error {
	return errors.New("waiting for an exit without reaping is not supported here")
}
