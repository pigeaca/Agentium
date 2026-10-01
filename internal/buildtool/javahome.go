package buildtool

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolveJavaHome finds a JDK on the host, for the agent's JAVA_HOME. The sandbox cannot look one up itself: per-user
// JDKs (under ~/Library/Java on macOS) are found through /usr/libexec/java_home, which fails inside it, and the agent
// would find no JDK at all. Order: the user's JAVA_HOME when it holds a JDK; macOS's java_home; the folder above the
// bin/ of the first `java` on PATH. It returns "" when none is found. run is exec.Command's output, a parameter so tests
// need no JDK.
func ResolveJavaHome(environ []string, run func(name string, args ...string) (string, error)) string {
	env := vars(environ)
	if isJDK(env["JAVA_HOME"]) {
		return env["JAVA_HOME"]
	}
	if runtime.GOOS == "darwin" && run != nil {
		if out, err := run("/usr/libexec/java_home"); err == nil && isJDK(strings.TrimSpace(out)) {
			return strings.TrimSpace(out)
		}
	}
	for _, dir := range filepath.SplitList(env["PATH"]) {
		java := filepath.Join(dir, "java")
		resolved, err := filepath.EvalSymlinks(java)
		if err != nil {
			continue
		}
		if home := filepath.Dir(filepath.Dir(resolved)); filepath.Base(filepath.Dir(resolved)) == "bin" && isJDK(home) {
			return home
		}
	}
	return ""
}

// isJDK reports whether dir is an absolute folder with bin/java.
func isJDK(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "bin", "java"))
	return err == nil && info.Mode().IsRegular()
}

// CommandOutput is exec.Command(name, args...).Output() as a string, for ResolveJavaHome.
func CommandOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
