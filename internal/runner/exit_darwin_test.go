package runner

import (
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// started starts argv as the test's own child, reaped at the test's end.
func started(t *testing.T, argv ...string) int {
	t.Helper()
	c := exec.Command(argv[0], argv[1:]...)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Process.Kill(); c.Wait() })
	return c.Process.Pid
}

// A fresh attach is the direct check: it fails on a process that has exited (a zombie, not yet reaped), and
// succeeds on one that runs; waitExit then returns at once for the zombie.
func TestAttachTellsAZombie(t *testing.T) {
	kq, err := syscall.Kqueue()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(kq)
	zombie := started(t, "/usr/bin/true")
	for i := 0; i < 200 && leaderState(zombie) != "Z"; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if state := leaderState(zombie); state != "Z" {
		t.Fatalf("the child is %q, not a zombie", state)
	}
	if exited, err := (kqueueExit{kq: kq, pid: zombie}).attach(); err != nil || !exited {
		t.Errorf("attach to a zombie: exited %v, %v", exited, err)
	}
	start := time.Now()
	if err := waitExit(zombie); err != nil || time.Since(start) > 500*time.Millisecond {
		t.Errorf("waitExit on a zombie: %v after %v", err, time.Since(start))
	}
	running := started(t, "/bin/sleep", "30")
	w := kqueueExit{kq: kq, pid: running}
	if exited, err := w.attach(); err != nil || exited {
		t.Errorf("attach to a running process: exited %v, %v", exited, err)
	}
	w.detach()
	if state := leaderState(zombie); state != "Z" {
		t.Errorf("the zombie was reaped: %q", state)
	}
}

// lostEvent is a real watch whose exit event never comes.
type lostEvent struct{ kqueueExit }

func (l lostEvent) event(timeout time.Duration) (bool, error) {
	time.Sleep(timeout)
	return false, nil
}

// An exit whose event is never delivered still ends the wait: after each bounded wait, a fresh attach finds the
// process exited.
func TestAwaitExitWithoutTheEvent(t *testing.T) {
	kq, err := syscall.Kqueue()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(kq)
	pid := started(t, "/bin/sleep", "0.3")
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- awaitExit(lostEvent{kqueueExit{kq: kq, pid: pid}}, 50*time.Millisecond) }()
	select { // a regression is a named failure, not a hang until go test's timeout
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait never ended: an exit whose event is lost is never seen")
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > 3*time.Second {
		t.Errorf("the wait ended after %v: want soon after the process's exit at 300 ms", took)
	}
	if state := leaderState(pid); state != "Z" {
		t.Errorf("process %s is %q: want exited and not reaped", strconv.Itoa(pid), state)
	}
}
