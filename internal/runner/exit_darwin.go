package runner

import (
	"errors"
	"syscall"
	"time"
)

// canWaitWithoutReaping: whether this system's waitExit works; without it Run refuses to start a command.
const canWaitWithoutReaping = true

// waitExit returns once the child process pid has exited, without reaping it (kqueue's NOTE_EXIT): it stays a zombie,
// holding its ID, until cmd.Wait.
func waitExit(pid int) error {
	kq, err := syscall.Kqueue()
	if err != nil {
		return err
	}
	defer syscall.Close(kq)
	return awaitExit(kqueueExit{kq: kq, pid: pid}, time.Second)
}

// exitWatch is how awaitExit watches one process: attach starts watching it, or reports that it has exited already;
// event waits up to timeout for its exit; detach stops watching it.
type exitWatch interface {
	attach() (exited bool, err error)
	event(timeout time.Duration) (exited bool, err error)
	detach()
}

// awaitExit waits for w's process to exit. Each wait for the event is bounded; after each, the watch is removed and
// attached afresh, which checks the process directly: XNU refuses to attach to a process that has exited (a zombie)
// with ESRCH. Repeating an attach on a watch that is already there would not (it only updates the watch's flags), so
// an exit event that was never delivered would otherwise be waited for for ever.
func awaitExit(w exitWatch, timeout time.Duration) error {
	for {
		exited, err := w.attach()
		if err != nil || exited {
			return err
		}
		if exited, err := w.event(timeout); err != nil || exited {
			return err
		}
		w.detach()
	}
}

// kqueueExit watches process pid on kqueue kq.
type kqueueExit struct{ kq, pid int }

func (k kqueueExit) change(flags uint16) syscall.Kevent_t {
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, k.pid, syscall.EVFILT_PROC, int(flags))
	ev.Fflags = syscall.NOTE_EXIT
	return ev
}

func (k kqueueExit) attach() (bool, error) {
	for {
		_, err := syscall.Kevent(k.kq, []syscall.Kevent_t{k.change(syscall.EV_ADD | syscall.EV_ONESHOT)}, nil, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ESRCH): // a zombie: only Run reaps it, so it has exited
			return true, nil
		}
		return false, err
	}
}

func (k kqueueExit) event(timeout time.Duration) (bool, error) {
	ts := syscall.NsecToTimespec(int64(timeout))
	events := make([]syscall.Kevent_t, 1)
	n, err := syscall.Kevent(k.kq, nil, events, &ts)
	switch {
	case errors.Is(err, syscall.EINTR):
		return false, nil
	case err != nil:
		return false, err
	case n > 0 && events[0].Flags&syscall.EV_ERROR != 0 && syscall.Errno(events[0].Data) != syscall.ESRCH:
		return false, syscall.Errno(events[0].Data)
	}
	return n > 0, nil
}

func (k kqueueExit) detach() {
	_, _ = syscall.Kevent(k.kq, []syscall.Kevent_t{k.change(syscall.EV_DELETE)}, nil, nil) // gone already (fired): fine
}
