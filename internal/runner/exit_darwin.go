package runner

import (
	"errors"
	"syscall"
	"time"
)

// waitExit returns once the child process pid has exited, without reaping it (kqueue's NOTE_EXIT): it stays a zombie,
// holding its ID, until cmd.Wait. A child that has already exited cannot be registered (ESRCH): it is a zombie, since
// only Run reaps it. Each wait is bounded, and the registration repeated, so an exit is never missed.
func waitExit(pid int) error {
	kq, err := syscall.Kqueue()
	if err != nil {
		return err
	}
	defer syscall.Close(kq)
	var change syscall.Kevent_t
	syscall.SetKevent(&change, pid, syscall.EVFILT_PROC, syscall.EV_ADD|syscall.EV_ONESHOT)
	change.Fflags = syscall.NOTE_EXIT
	timeout := syscall.NsecToTimespec(int64(time.Second))
	events := make([]syscall.Kevent_t, 1)
	for {
		if _, err := syscall.Kevent(kq, []syscall.Kevent_t{change}, nil, nil); errors.Is(err, syscall.ESRCH) {
			return nil
		} else if err != nil && !errors.Is(err, syscall.EINTR) {
			return err
		}
		n, err := syscall.Kevent(kq, nil, events, &timeout)
		switch {
		case err != nil && !errors.Is(err, syscall.EINTR):
			return err
		case n > 0 && events[0].Flags&syscall.EV_ERROR != 0 && syscall.Errno(events[0].Data) != syscall.ESRCH:
			return syscall.Errno(events[0].Data)
		case n > 0:
			return nil
		}
	}
}
