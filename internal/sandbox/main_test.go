package sandbox

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// helperVar makes the test binary a probe instead of running tests, so sandboxed tests need no tool beyond the binary
// itself: SANDBOX_TEST_HELPER=<mode> with the mode's arguments after "--".
const helperVar = "SANDBOX_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperVar); mode != "" {
		os.Exit(helper(mode, os.Args[len(os.Args)-1]))
	}
	os.Exit(m.Run())
}

// helper runs one probe and returns its exit code: 0 when the operation succeeded, 3 when it failed (the error goes to
// stderr).
//   - dial <addr>: a TCP connection to addr, 2 seconds at most;
//   - serve <addr>: listen on addr (tcp), then connect to the listening address from the same process;
//   - listen <addr>: listen on addr (tcp) only;
//   - shm <name>, sem <name>: create a POSIX shared memory object or semaphore of that name, then remove it.
func helper(mode, arg string) int {
	var err error
	switch mode {
	case "listen":
		var l net.Listener
		if l, err = net.Listen("tcp", arg); err == nil {
			l.Close()
		}
	case "shm", "sem":
		err = ipcProbe(mode, arg)
	case "dial":
		var c net.Conn
		if c, err = net.DialTimeout("tcp", arg, 2*time.Second); err == nil {
			c.Close()
		}
	case "serve":
		var l net.Listener
		if l, err = net.Listen("tcp", arg); err == nil {
			defer l.Close()
			go func() {
				if c, err := l.Accept(); err == nil {
					c.Close()
				}
			}()
			var c net.Conn
			if c, err = net.DialTimeout("tcp", l.Addr().String(), 2*time.Second); err == nil {
				c.Close()
			}
		}
	default:
		err = fmt.Errorf("unknown helper mode %q", mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	return 0
}
