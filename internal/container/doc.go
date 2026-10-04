// Package container is Agentium's Docker driver for container grading (mode container-v1): it calls the docker CLI
// directly, with argv only and never a shell, and owns the grade's container from creation to removal.
//
//   - The client (Open): the daemon must be local (a Unix socket; a remote endpoint would send hidden tests off the
//     machine) and is pinned with --host for every later call; it must run Linux with seccomp and cgroup v2 and a
//     recent enough API. Fits compares the limits with the daemon's memory and CPUs, and Image finds an image by
//     digest and never pulls it.
//   - The grade's container (Run): created in a fixed shape (no network, a private IPC namespace, every capability
//     dropped, no new privileges, user 65534, a read-only root, one anonymous volume at /grade, a sized /tmp, no logs,
//     resource limits, no host mounts and never the Docker socket, a main process that sleeps until the deadline,
//     --rm), checked against the daemon's own record before it starts (the inspect check, with a normalized digest),
//     given its work/ and cache/ folders by a trusted two-entry tar, started, and proved isolated from inside (the
//     probes) before any of the grade's code runs. Run removes it whatever happens: on success, error, cancel and
//     panic.
//   - Inside (Container): the grading copy goes in as a tar stream written here (links are never followed and the
//     host copy is only read) and unpacked by the image's own tar as the grade's user; commands run with a timeout,
//     their output to a writer, and the cgroup counters (OOM kills, process-limit hits) are read after each. A timeout
//     or a cancel kills the container, since killing the docker client leaves its processes running.
//   - Leftovers: every container and its volume carry labels that tie them to the data folder and the run, so
//     recovery and clean find them (Leftovers, RemoveRun), including a container created but never started, which
//     neither --rm nor the deadline removes.
//
// Nothing here pulls or builds an image, starts a daemon or changes Docker's settings or contexts.
package container
