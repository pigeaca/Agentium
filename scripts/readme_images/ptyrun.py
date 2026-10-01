"""Run a command with stdout and stderr on a pseudo-terminal (width from COLS, default 160); print what it wrote."""
import fcntl, os, struct, subprocess, sys, termios
master, slave = os.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 50, int(os.environ.get("COLS", "160")), 0, 0))
proc = subprocess.Popen(sys.argv[1:], stdin=subprocess.DEVNULL, stdout=slave, stderr=slave, close_fds=True)
os.close(slave)
out = b""
while True:
    try:
        data = os.read(master, 65536)
    except OSError:
        break
    if not data:
        break
    out += data
proc.wait()
sys.stdout.buffer.write(out.replace(b"\r\n", b"\n"))
sys.exit(proc.returncode)
