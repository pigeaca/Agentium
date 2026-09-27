# Dependencies

Every new package, upgrade, tool or toolchain installation requires explicit approval. Check necessity, maintenance, license and security before proposing one. Preserve lockfiles; harness commands never download. The one sanctioned install is `harness.py worktree new`/`worktree deps`: an offline, lockfile-exact install from the local package store, which fails rather than fetching. Report missing tools instead of silently installing them. Pin CI actions to full commit SHAs, and pin tools and base images to exact versions.
