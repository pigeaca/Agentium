package buildtool

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The project's own distribution metadata, without the project.
//
// The venv never holds the project (see pythonProfile), so a test that asks importlib.metadata for the project's
// version (click's test_deprecations at import, attrs' test_packaging, marshmallow's version test) fails with
// PackageNotFoundError, for the agent, validation and grading alike. So each base gets a .dist-info of its own that
// holds METADATA and nothing else, in a read-only folder of the deps folder (<deps>/py-meta/<key>), which the
// environment puts on PYTHONPATH after the checkout's import root: importlib.metadata finds the version there, and
// every import still resolves to the checkout, which comes first, while the folder holds no module (a folder named
// <name>-<version>.dist-info cannot be imported). No RECORD, top_level.txt or entry_points.txt is kept: nothing can point
// an import, a console script or a plugin at other code.
//
// How it is made: the build backend, through the standard front end of the project's manager, in the warm-up's
// throwaway checkout of the base (with network, under the warm-up lock; the trusted-repository rule): `uv build
// --wheel` for uv, the venv's `pip wheel --no-deps` for pip, into a scratch folder in the denied py-resolve folder.
// Both run PEP 517's hooks with build isolation, in-tree backends and the backend's extra requirements, which a
// call of prepare_metadata_for_build_wheel by hand would have to redo, and a backend without that hook (it is optional)
// is built instead anyway. The wheel's METADATA is read and the wheel removed. The base has none of its own task's
// hidden tests or reference solution, and only the METADATA's headers are kept: the long description (the body, or a
// Description header), which may quote the README or a changelog, is dropped.
//
// Versions computed from git (setuptools-scm, hatch-vcs, pdm-backend, the dynamic-versioning plugins) cannot be read
// from a checkout of one commit with no tags: setuptools-scm would say 0.1.dev1, which a project's own version parser
// may refuse (attrs' VersionInfo wants three numbers). The build runs with each backend's override set to
// pythonPretendVersion, which a project with a static version ignores; the run gets a note when it was used.
//
// The folder is named by a hash of what it holds, so bases with the same metadata share one, and the base's stamp
// (the run's, per base commit) names it; MetadataReady checks it before a run is handed it.

// pythonPretendVersion is the version a project whose version comes from git gets: three numbers, so a project's own
// parser of its version reads it (attrs' VersionInfo; "0+unknown" would fail it).
const pythonPretendVersion = "0.0.0"

// pretendVersionVars are the build backends' overrides of a version computed from git: setuptools-scm's (hatch-vcs and
// flit-scm read it too), pdm-backend's, poetry-dynamic-versioning's and uv-dynamic-versioning's.
var pretendVersionVars = []string{"SETUPTOOLS_SCM_PRETEND_VERSION", "PDM_BUILD_SCM_VERSION", "POETRY_DYNAMIC_VERSIONING_BYPASS",
	"UV_DYNAMIC_VERSIONING_BYPASS"}

// metadataRecipe is part of every metadata folder's key: changing what a folder holds changes it.
const metadataRecipe = "py-meta-1"

// maxMetadata bounds the METADATA read from a wheel.
const maxMetadata = 16 << 20

// metadataVersion is a version that can name a .dist-info folder: PEP 440's characters, no "-" (the name's separator).
var metadataVersion = regexp.MustCompile(`^[A-Za-z0-9.+!_]+$`)

// metadataKey is a header's name as the core metadata format writes it.
var metadataKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

// maxMetadataTries is how many warm-ups of a base try to make its metadata before the base is stamped without it.
const maxMetadataTries = 3

// withMetadata adds the base's metadata folder (projectMetadata) to what the venv's warm-up found, and its note. A
// failure that may be worth another try is Failed and Transient, with the venv: the base is not stamped, so the next
// warm-up tries again, while this run still gets the venv (see Warmed).
func withMetadata(ctx context.Context, in WarmInput, inputs pyInputs, uv, interp string, env []string, w Warmed) (Warmed, error) {
	dir, note, retry, err := projectMetadata(ctx, in, inputs, uv, interp, w.Venv, env)
	if err != nil {
		return Warmed{}, err
	}
	w.Metadata = dir
	switch {
	case retry:
		w.Failed, w.Transient = note, true
	case note != "":
		w.Notes = append(slices.Clone(w.Notes), note)
	}
	return w, nil
}

// projectMetadata makes the base's metadata folder, or finds it made (see the top of this file): its path, or "" with
// a note when the build or its METADATA fails (the run goes on: only tests that read the project's metadata fail). A
// failure may be a download's (the build backend), so it is retried (retry) by the next maxMetadataTries-1 warm-ups of
// the base, counted in in.State; the last one's note is stamped. A project that is no package (no build system for uv,
// `[tool.uv] package = false`) has none, and no note. Errors are for cancellation and the deps folder's I/O. env is the
// venv's warm-up environment (caches in the deps folder).
func projectMetadata(ctx context.Context, in WarmInput, inputs pyInputs, uv, interp, venv string, env []string) (dir, note string, retry bool, err error) {
	if !inputs.pkg {
		return "", "", false, nil
	}
	scratch := filepath.Join(in.Deps, "py-resolve") // denied to agents: the wheel holds the base's code
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return "", "", false, err
	}
	// Wheels a killed warm-up left: no other warm-up is building one (the lock is held).
	stale, _ := filepath.Glob(filepath.Join(scratch, "wheel-*"))
	for _, s := range stale {
		if err := os.RemoveAll(s); err != nil {
			return "", "", false, err
		}
	}
	out, err := os.MkdirTemp(scratch, "wheel-")
	if err != nil {
		return "", "", false, err
	}
	defer os.RemoveAll(out)
	args := []string{uv, "build", "--wheel", "--out-dir", out, "--python", interp}
	if inputs.manager != "uv" {
		args = []string{filepath.Join(venv, "bin", "python"), "-m", "pip", "wheel", "--quiet", "--no-deps", "--wheel-dir", out, "."}
	}
	build := slices.Clone(env)
	for _, name := range pretendVersionVars {
		build = append(build, name+"="+pythonPretendVersion)
	}
	tries := ""
	if in.State != "" && in.Base != "" {
		tries = filepath.Join(in.State, filepath.Base(in.Base)+".metadata-tries")
	}
	failed := func(why string) (string, string, bool, error) {
		note := "the project's metadata could not be made (" + why + "): its tests cannot ask importlib.metadata for the project's version"
		if tries == "" {
			return "", note, true, nil
		}
		n := 0
		if data, err := os.ReadFile(tries); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if n++; n >= maxMetadataTries {
			// Stamped with the note; the count starts again, so removing the base's stamp gives it as many tries.
			if err := os.Remove(tries); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", "", false, err
			}
			return "", fmt.Sprintf("%s (%d tries)", note, n), false, nil
		}
		if err := WriteFileSynced(tries, []byte(strconv.Itoa(n)), 0o600); err != nil {
			return "", "", false, err
		}
		return "", fmt.Sprintf("%s (try %d of %d)", note, n, maxMetadataTries), true, nil
	}
	ok, err := pyRun(ctx, in, args, build)
	if ctx.Err() != nil {
		return "", "", false, ctx.Err()
	}
	if err != nil || !ok {
		return failed("the wheel's build failed: see setup.log")
	}
	name, version, distInfo, content, err := wheelMetadata(out)
	if err != nil {
		return failed(err.Error())
	}
	if dir, err = installMetadata(in, distInfo, content); err != nil {
		return "", "", false, fmt.Errorf("the project's metadata: %w", err)
	}
	if tries != "" {
		if err := os.Remove(tries); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", "", false, err
		}
	}
	fmt.Fprintf(in.Log, "[agentium] the project's metadata (%s %s, headers only): %s\n", name, version, dir)
	if version == pythonPretendVersion && inputs.version != pythonPretendVersion {
		note = "the project's version could not be read statically (a version computed from git needs history, which a checkout of " +
			"one commit lacks): its metadata says " + pythonPretendVersion
	}
	return dir, note, false, nil
}

// distInfoName is a .dist-info folder's name: <name>-<version>.dist-info, one path segment.
var distInfoName = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._]*)-([A-Za-z0-9.+!_]+)\.dist-info$`)

// wheelMetadata reads the one wheel in dir and returns its METADATA's name, version and headers (metadataHeaders),
// and the wheel's own .dist-info folder name, which the metadata folder keeps as an installer would: importlib.metadata
// finds a dotted name (zope.interface) by the form the wheel wrote, and Python 3.9 not by another. The name must be
// one path segment of the form <name>-<version>.dist-info that matches METADATA's name and version.
func wheelMetadata(dir string) (name, version, distInfo string, headers []byte, err error) {
	wheels, err := filepath.Glob(filepath.Join(dir, "*.whl"))
	if err != nil || len(wheels) != 1 {
		return "", "", "", nil, fmt.Errorf("the build made %d wheels, not one", len(wheels))
	}
	zr, err := zip.OpenReader(wheels[0])
	if err != nil {
		return "", "", "", nil, fmt.Errorf("the wheel: %w", err)
	}
	defer zr.Close()
	var found *zip.File
	for _, f := range zr.File {
		parts := strings.Split(f.Name, "/")
		if len(parts) == 2 && strings.HasSuffix(parts[0], ".dist-info") && parts[1] == "METADATA" {
			if found != nil {
				return "", "", "", nil, errors.New("the wheel has two METADATA files")
			}
			found, distInfo = f, parts[0]
		}
	}
	if found == nil || found.UncompressedSize64 > maxMetadata {
		return "", "", "", nil, errors.New("the wheel has no METADATA under 16 MiB")
	}
	rc, err := found.Open()
	if err != nil {
		return "", "", "", nil, fmt.Errorf("the wheel's METADATA: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxMetadata+1))
	if err != nil || len(data) > maxMetadata {
		return "", "", "", nil, fmt.Errorf("the wheel's METADATA: %v", err)
	}
	if name, version, headers, err = metadataHeaders(data); err != nil {
		return "", "", "", nil, err
	}
	m := distInfoName.FindStringSubmatch(distInfo)
	if m == nil || normalizeName(m[1]) != normalizeName(name) || m[2] != version {
		return "", "", "", nil, fmt.Errorf("the wheel's folder %q does not name %s %s", distInfo, name, version)
	}
	return name, version, distInfo, headers, nil
}

// metadataHeaders keeps a METADATA file's headers (each with its continuation lines) but the long description: the
// body after the first empty line and a Description header. It returns the name and version, which must be ones that
// can name a .dist-info folder.
func metadataHeaders(data []byte) (name, version string, headers []byte, err error) {
	var b strings.Builder
	keep := false
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if line == "" {
			break // the body: the long description
		}
		if line[0] == ' ' || line[0] == '\t' {
			if keep {
				b.WriteString(line + "\n")
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || !metadataKey.MatchString(key) {
			return "", "", nil, fmt.Errorf("the wheel's METADATA has a line that is no header: %.60q", line)
		}
		if keep = !strings.EqualFold(key, "Description"); !keep {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case strings.EqualFold(key, "Name") && name == "":
			name = value
		case strings.EqualFold(key, "Version") && version == "":
			version = value
		}
		b.WriteString(line + "\n")
	}
	if !pyName.MatchString(name) || !metadataVersion.MatchString(version) {
		return "", "", nil, fmt.Errorf("the wheel's METADATA names %q version %q", name, version)
	}
	return name, version, []byte(b.String()), nil
}

// metadataFolderKey names a metadata folder by what it holds.
func metadataFolderKey(distInfo string, content []byte) string {
	sum := sha256.Sum256([]byte(metadataRecipe + "\x00" + distInfo + "\x00" + string(content)))
	return fmt.Sprintf("%x", sum[:8])
}

// installMetadata puts <distInfo>/METADATA into <deps>/py-meta/<key>, read-only, or finds it there. It runs under the
// warm-up lock, so no other warm-up writes the folder meanwhile, while agents of other runs may read it: a folder is
// made whole under a temporary name and renamed, never changed after; one that is no longer as made (MetadataReady)
// is moved aside, never removed, as a stamped venv is, since a run may have it on its PYTHONPATH.
func installMetadata(in WarmInput, distInfo string, content []byte) (string, error) {
	root := filepath.Join(in.Deps, "py-meta")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	// What a warm-up that died left: no stamp names it, and no other warm-up is writing one (the lock).
	leftovers, _ := filepath.Glob(filepath.Join(root, ".new-*"))
	for _, l := range leftovers {
		if err := setWritable(l, true); err != nil {
			return "", err
		}
		if err := os.RemoveAll(l); err != nil {
			return "", err
		}
	}
	final := filepath.Join(root, metadataFolderKey(distInfo, content))
	if MetadataReady(final) {
		return final, nil
	}
	if fileExists(final) {
		aside, err := moveAside(final, in.Now)
		if err != nil {
			return "", fmt.Errorf("move a changed metadata folder aside: %w", err)
		}
		fmt.Fprintf(in.Log, "[agentium] the metadata folder %s is no longer as made: moved aside to %s (remove it when no run uses it) and made again\n", final, aside)
	}
	tmp, err := os.MkdirTemp(root, ".new-")
	if err != nil {
		return "", err
	}
	folder := filepath.Join(tmp, distInfo)
	if err := os.Mkdir(folder, 0o700); err != nil {
		return "", err
	}
	if err := WriteFileSynced(filepath.Join(folder, "METADATA"), content, 0o444); err != nil {
		return "", err
	}
	if err := os.Chmod(folder, 0o555); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	// Read-only last: a crash before it leaves a writable folder, which MetadataReady refuses and the next warm-up
	// moves aside.
	if err := os.Chmod(final, 0o555); err != nil {
		return "", err
	}
	if err := syncDir(root); err != nil {
		return "", err
	}
	return final, nil
}

// MetadataReady reports whether dir is a finished metadata folder (installMetadata): one .dist-info folder holding one
// regular file, METADATA, all read-only and real (no link), whose name and content hash to the folder's name. A
// base's stamp naming a folder that is not ready is warmed again.
func MetadataReady(dir string) bool {
	readOnly := func(p string, dirWanted bool) bool {
		info, err := os.Lstat(p)
		return err == nil && info.Mode().Perm()&0o222 == 0 && (dirWanted && info.IsDir() || !dirWanted && info.Mode().IsRegular())
	}
	if dir == "" || !readOnly(dir, true) {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".dist-info") {
		return false
	}
	distInfo := filepath.Join(dir, entries[0].Name())
	files, err := os.ReadDir(distInfo)
	if err != nil || len(files) != 1 || files[0].Name() != "METADATA" || !readOnly(distInfo, true) ||
		!readOnly(filepath.Join(distInfo, "METADATA"), false) {
		return false
	}
	content, err := os.ReadFile(filepath.Join(distInfo, "METADATA"))
	return err == nil && metadataFolderKey(entries[0].Name(), content) == filepath.Base(dir)
}

// moveAside renames a folder that is no longer as made, whole, to <path>.bad-<time> (with a number when that exists)
// and returns the new name. A read-only folder cannot be renamed (macOS asks for write permission on a folder that
// moves): its own mode is opened for the rename and put back after, so what a run may still read stays read-only.
func moveAside(path string, now time.Time) (string, error) {
	stamp := now.UTC().Format("20060102T150405Z")
	aside := fmt.Sprintf("%s.bad-%s", path, stamp)
	for n := 2; fileExists(aside); n++ { // another rebuild in the same second
		aside = fmt.Sprintf("%s.bad-%s-%d", path, stamp, n)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		if err := os.Chmod(path, info.Mode().Perm()|0o200); err != nil {
			return "", err
		}
	}
	if err := os.Rename(path, aside); err != nil {
		os.Chmod(path, info.Mode().Perm())
		return "", err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		if err := os.Chmod(aside, info.Mode().Perm()); err != nil {
			return "", err
		}
	}
	return aside, nil
}

// syncDir makes a folder's entries durable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
