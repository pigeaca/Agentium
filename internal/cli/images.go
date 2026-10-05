package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pigeaca/agentium/internal/container"
	"github.com/pigeaca/agentium/internal/gitx"
	"github.com/pigeaca/agentium/internal/home"
	"github.com/pigeaca/agentium/internal/store"
	"github.com/pigeaca/agentium/internal/term"
)

const imagesUsage = `Usage: agentium images [list | pull | remove] [flags] [TOOLCHAIN...]

The images container grading runs in (--grader container, not available yet). Each is built on this machine from an
official base image pinned by digest, adding git and less through apt; Agentium never pulls or builds one without your
consent, and never by tag. TOOLCHAIN is go, jdk, rust or python.

  agentium images [list]                       the pinned images: present or not, their sizes, and which this
                                               project needs (in your registered repository)
  agentium images pull [--yes] [TOOLCHAIN...]  shows what it would download (base images and apt packages), asks,
                                               then pulls the bases by digest, builds and checks the images; without
                                               a TOOLCHAIN, the ones this project needs
  agentium images remove [--yes] [--bases] [TOOLCHAIN...]
                                               removes the grading images (with --bases, the pulled bases too); without
                                               --yes it only shows what would go

Flags:
  --yes    pull: agree to the download without being asked; remove: remove what it lists
  --bases  remove: the pinned bases too, not only the grading images built from them
`

func runImages(ctx context.Context, env Env, args []string) int {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list":
		return imagesList(ctx, env, args)
	case "pull":
		return imagesPull(ctx, env, args)
	case "remove":
		return imagesRemove(ctx, env, args)
	case "help":
		fmt.Fprint(env.Stdout, imagesUsage)
		return ExitOK
	}
	fmt.Fprintf(env.Stderr, "agentium images: unknown subcommand %q\n\n%s", sub, imagesUsage)
	return ExitUsage
}

// imagePins are the pins named on the command line, in the table's order; an unknown name is a usage error.
func imagePins(env Env, cmd string, names []string) ([]container.Pin, bool) {
	var out []container.Pin
	for _, p := range container.Pins() {
		if slices.Contains(names, p.Toolchain) {
			out = append(out, p)
		}
	}
	for _, n := range names {
		if _, ok := container.PinFor(n); !ok {
			fmt.Fprintf(env.Stderr, "agentium images %s: unknown toolchain %q: give go, jdk, rust or python\n", cmd, n)
			return nil, false
		}
	}
	return out, true
}

// projectNeeds says, per toolchain, whether the project in the current folder needs its image: "yes", or why its
// image cannot grade it (the version match, open decision 6). ok is false outside a registered project.
func projectNeeds(ctx context.Context, env Env) (needs map[string]string, pins []container.Pin, matchErr error, ok bool) {
	w, err := findProjectReadOnly(ctx, env)
	if err != nil || w == nil {
		return nil, nil, nil, false
	}
	defer w.Close()
	tools, err := w.buildTools(ctx)
	if err != nil {
		return nil, nil, nil, false
	}
	host, err := w.hostToolchain(ctx, env)
	if err != nil {
		return nil, nil, nil, false
	}
	needs = map[string]string{}
	for _, tool := range tools {
		tc := container.ToolchainOf(tool)
		if tc == "" {
			continue
		}
		if _, err := container.Match([]string{tool}, host); err != nil {
			needs[tc] = strings.TrimPrefix(err.Error(), container.ErrNoImage.Error()+": ")
		} else if needs[tc] == "" {
			needs[tc] = "yes"
		}
	}
	pins, matchErr = container.Match(tools, host)
	return needs, pins, matchErr, true
}

// findProjectReadOnly finds the registered project of the current folder and writes nothing: no data folder is made,
// and the database is only read (store.OpenReadOnly: no migration, no side files). A folder outside a repository, a
// data folder or database that does not exist, and a repository not registered are no project (nil, nil).
func findProjectReadOnly(ctx context.Context, env Env) (*workspace, error) {
	if env.Dir == "" {
		return nil, nil
	}
	root, err := gitx.Run(ctx, "-C", env.Dir, "rev-parse", "--show-toplevel")
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, nil
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, nil
	}
	layout, err := home.Resolve(env.Getenv)
	if err != nil || layout.CheckOutside(root) != nil {
		return nil, nil
	}
	if info, err := os.Stat(layout.Database); err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	db, err := store.OpenReadOnly(ctx, layout.Database)
	if err != nil {
		return nil, err
	}
	project, err := db.ProjectByRoot(ctx, root)
	if err != nil {
		db.Close()
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	env.noteRoot(root)
	return &workspace{db: db, project: project, layout: layout, root: root, bare: layout.ProjectRepo(project.ID)}, nil
}

func imagesList(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("images list", flag.ContinueOnError)
	rest, code, ok := parseArgs(env, fs, args, imagesUsage)
	if !ok {
		return code
	}
	if len(rest) > 0 {
		fmt.Fprintf(env.Stderr, "agentium images list: takes no arguments, got %q\n\n%s", rest[0], imagesUsage)
		return ExitUsage
	}
	layout, err := home.Resolve(env.Getenv)
	if err != nil {
		return fail(env, err)
	}
	d, err := env.docker(ctx)
	if err != nil {
		return fail(env, fmt.Errorf("the images need Docker: %w", err))
	}
	defer d.Close()
	built, err := container.LoadBuilt(builtImagesPath(layout))
	if err != nil {
		return fail(env, err)
	}
	plan, err := d.Plan(ctx, container.Pins(), built)
	if err != nil {
		return fail(env, err)
	}
	local, err := d.LocalImages(ctx)
	if err != nil {
		return fail(env, err)
	}
	needs, _, _, inProject := projectNeeds(ctx, env)
	printImages(env, plan, local, needs, inProject)
	return ExitOK
}

// printImages prints images list: a row per pin (its base, its grading image and, in a project, whether the project
// needs it), then notes: grading images of an earlier recipe, what is still to confirm about a pin, and how to pull.
func printImages(env Env, plan container.ImagePlan, local []container.LocalImage, needs map[string]string, inProject bool) {
	st := env.style()
	out := env.Stdout
	fmt.Fprintln(out, st.Heading(fmt.Sprintf("Grading images (linux/%s): each built here from its pinned base, adding %s", plan.Arch, packagesText())))
	cols := []term.Column{term.Left("toolchain"), term.Left("base (pinned by digest)"), term.Left("base"), term.Left("grading image")}
	if inProject {
		cols = append(cols, term.Left("this project"))
	}
	table := term.NewTable(st, cols...)
	table.Indent = "  "
	sizes := map[string]int64{}
	for _, l := range local {
		sizes[l.Ref] = l.Size
	}
	for _, it := range plan.Items {
		base := "missing: " + formatBytes(it.BaseSize) + " to pull"
		switch {
		case it.BaseErr != nil:
			base = st.Warn("not the pinned content")
		case it.BasePresent:
			base = "present, " + formatBytes(sizes[it.Recipe.Base])
		}
		grading := fmt.Sprintf("not built (apt: about %s)", formatBytes(it.Pin.AptEstimate))
		if it.Ready {
			grading = "built, " + formatBytes(sizes[it.Built.Tag]) + " (" + it.Built.Inside + ")"
		}
		row := []string{it.Pin.Name(), it.Pin.Short(plan.Arch), base, grading}
		if inProject {
			need := needs[it.Pin.Toolchain]
			if need == "" {
				need = "no"
			}
			row = append(row, need)
		}
		table.Row(row...)
	}
	_ = table.Write(out)
	var others []string
	for _, l := range local {
		if l.Kind == "grading" && !l.Current {
			others = append(others, fmt.Sprintf("%s (%s)", l.Ref, formatBytes(l.Size)))
		}
	}
	if len(others) > 0 {
		fmt.Fprintln(out, note(st, "grading images of an earlier recipe: "+strings.Join(others, ", ")+"; agentium images remove --yes removes them"))
	}
	for _, it := range plan.Items {
		if it.Pin.Unconfirmed != "" {
			fmt.Fprintln(out, note(st, it.Pin.Name()+": still to confirm: "+it.Pin.Unconfirmed))
		}
		if it.BaseErr != nil {
			fmt.Fprintln(out, warning(st, it.Pin.Name()+": "+it.BaseErr.Error()))
		}
	}
	if pending := plan.Pending(); len(pending) > 0 {
		fmt.Fprintf(out, "\n%s shows the download and asks before it pulls or builds anything.\n", st.Command("agentium images pull"))
	}
}

func imagesPull(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("images pull", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "agree to the download without being asked")
	rest, code, ok := parseArgs(env, fs, args, imagesUsage)
	if !ok {
		return code
	}
	pins, code, ok := pullPins(ctx, env, rest)
	if !ok {
		return code
	}
	layout, err := home.Resolve(env.Getenv)
	if err != nil {
		return fail(env, err)
	}
	d, err := env.docker(ctx)
	if err != nil {
		return fail(env, fmt.Errorf("the images need Docker: %w", err))
	}
	defer d.Close()
	// Nothing is written before the consent: the plan only reads the data folder's record and the daemon.
	plan, built, err := imagePlan(ctx, d, layout, pins)
	if err != nil {
		return fail(env, err)
	}
	if len(plan.Pending()) == 0 {
		fmt.Fprintf(env.Stdout, "The grading images are ready: %s.\n", pinNames(plan.Items))
		return ExitOK
	}
	fmt.Fprint(env.Stdout, consentText(plan))
	if !*yes && !askYes(ctx, env, "Pull and build them?") {
		fmt.Fprintf(env.Stdout, "Nothing was downloaded. Run %s to agree, or answer yes at a terminal.\n",
			env.style().Command(strings.TrimSpace("agentium images pull --yes "+strings.Join(rest, " "))))
		return ExitError
	}
	if err := layout.Ensure(); err != nil {
		return fail(env, err)
	}
	// One pull at a time per data folder: its record of built images is written whole. Another pull may have built
	// some meanwhile, so the plan is read again; it may only shrink, never go beyond what was agreed to.
	unlock, err := home.LockFile(ctx, builtImagesPath(layout)+".lock", func() {
		fmt.Fprintln(env.Stdout, "waiting for another agentium images pull to finish")
	})
	if err != nil {
		return fail(env, err)
	}
	defer unlock()
	agreed := plan
	if plan, built, err = imagePlan(ctx, d, layout, pins); err != nil {
		return fail(env, err)
	}
	if err := withinConsent(plan, agreed); err != nil {
		return fail(env, err)
	}
	if err := fetchImages(ctx, env, d, layout, plan.Pending(), built); err != nil {
		return fail(env, err)
	}
	fmt.Fprintf(env.Stdout, "\nThe grading images are ready: %s.\n", pinNames(plan.Items))
	return ExitOK
}

// imagePlan reads the data folder's record of built images and the daemon's state of the pins; a base that is not
// the pinned content is an error.
func imagePlan(ctx context.Context, d DockerClient, layout home.Layout, pins []container.Pin) (container.ImagePlan, container.BuiltImages, error) {
	built, err := container.LoadBuilt(builtImagesPath(layout))
	if err != nil {
		return container.ImagePlan{}, nil, err
	}
	plan, err := d.Plan(ctx, pins, built)
	if err != nil {
		return container.ImagePlan{}, nil, err
	}
	for _, it := range plan.Items {
		if it.BaseErr != nil {
			return container.ImagePlan{}, nil, fmt.Errorf("%s: %w", it.Pin.Name(), it.BaseErr)
		}
	}
	return plan, built, nil
}

// withinConsent refuses a plan that downloads more than the one agreed to: an image not pending then, or a base
// present then and missing now.
func withinConsent(plan, agreed container.ImagePlan) error {
	for _, it := range plan.Pending() {
		i := slices.IndexFunc(agreed.Items, func(a container.PlanItem) bool { return a.Pin.Toolchain == it.Pin.Toolchain })
		if i < 0 || agreed.Items[i].Ready || !it.BasePresent && agreed.Items[i].BasePresent {
			return fmt.Errorf("%s: it changed since you agreed (its base was removed, say), so nothing more is downloaded: run agentium images pull again", it.Pin.Name())
		}
	}
	return nil
}

// pullPins are the pins images pull gets: the named ones, or without a name the ones the project in the current
// folder needs (the version match refuses the pull when its toolchain is not the host's). ok false: return code.
func pullPins(ctx context.Context, env Env, names []string) (pins []container.Pin, code int, ok bool) {
	if pins, ok = imagePins(env, "pull", names); !ok {
		return nil, ExitUsage, false
	}
	if len(names) > 0 {
		return pins, ExitOK, true
	}
	_, needed, matchErr, inProject := projectNeeds(ctx, env)
	switch {
	case !inProject:
		fmt.Fprintf(env.Stderr, "agentium images pull: name the toolchains (go, jdk, rust, python), or run it in your registered repository\n\n%s", imagesUsage)
		return nil, ExitUsage, false
	case matchErr != nil:
		return nil, fail(env, matchErr), false
	case len(needed) == 0:
		fmt.Fprintln(env.Stdout, "This project's build tools need no grading image.")
		return nil, ExitOK, false
	}
	return needed, ExitOK, true
}

// fetchImages pulls and builds the pending images (consent given), recording each as it is built.
func fetchImages(ctx context.Context, env Env, d DockerClient, layout home.Layout, pending []container.PlanItem, built container.BuiltImages) error {
	st := env.style()
	data := dataID(layout)
	for _, it := range pending {
		fmt.Fprintln(env.Stdout, st.Heading("\n"+it.Pin.Name()+": "+map[bool]string{true: "building", false: "pulling and building"}[it.BasePresent]))
		b, err := d.Fetch(ctx, it, data, env.Stdout)
		if err != nil {
			return fmt.Errorf("%s: %w", it.Pin.Name(), err)
		}
		built[b.Tag] = b
		if err := built.Save(builtImagesPath(layout)); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "%s: %s, image %s (%s; %s)\n", it.Pin.Name(), b.Tag, b.ID[:min(19, len(b.ID))], b.Inside, strings.Join(b.Packages, ", "))
	}
	return nil
}

func pinNames(items []container.PlanItem) string {
	var names []string
	for _, it := range items {
		names = append(names, it.Pin.Name())
	}
	return strings.Join(names, ", ")
}

// registry names where a pinned base comes from.
func registry(repository string) string {
	if host, _, ok := strings.Cut(repository, "/"); ok && strings.ContainsAny(host, ".:") {
		return host
	}
	return "Docker Hub"
}

// consentText is what images pull shows before it asks: per image, the base it pulls (by digest, with its compressed
// size and registry) or that the base is present, and the build's apt download; then the total.
func consentText(plan container.ImagePlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "agentium images pull would download, on this machine's Docker (linux/%s):\n", plan.Arch)
	for _, it := range plan.Pending() {
		action := "base present"
		if !it.BasePresent {
			action = fmt.Sprintf("pull %s (%s compressed, from %s)", it.Pin.Short(plan.Arch), formatBytes(it.BaseSize), registry(it.Pin.Repository))
		}
		fmt.Fprintf(&b, "  %-12s %s; then build it, installing %s through apt (about %s)\n", it.Pin.Name(), action, packagesText(), formatBytes(it.Pin.AptEstimate))
	}
	base, apt := plan.Download()
	fmt.Fprintf(&b, "Total: about %s: %s of base images (at most: layers Docker already has are not downloaded again) and about %s through apt "+
		"(an estimate: the package lists, and %s with what they need, from the Debian or Ubuntu archive).\n",
		formatBytes(base+apt), formatBytes(base), formatBytes(apt), packagesText())
	return b.String()
}

// packagesText is container.GradingPackages for people: "git and less".
func packagesText() string { return strings.ReplaceAll(container.GradingPackages, " ", " and ") }

// askYes asks a yes-or-no question, only when a person can answer (stdin and stdout are terminals); Ctrl-C while it
// waits counts as no.
func askYes(ctx context.Context, env Env, question string) bool {
	if !env.StdinTerminal || !env.Terminal || env.Stdin == nil {
		return false
	}
	fmt.Fprintf(env.Stdout, "%s [y/N] ", question)
	type answer struct {
		line string
		err  error
	}
	got := make(chan answer, 1)
	go func() { // a blocked read cannot be cancelled: the process ends soon after, so the goroutine is left to it
		line, err := bufio.NewReader(io.LimitReader(env.Stdin, 1<<10)).ReadString('\n')
		got <- answer{line, err}
	}()
	select {
	case <-ctx.Done():
		return false
	case a := <-got:
		if a.err != nil && a.line == "" {
			return false
		}
		reply := strings.ToLower(strings.TrimSpace(a.line))
		return reply == "y" || reply == "yes"
	}
}

func imagesRemove(ctx context.Context, env Env, args []string) int {
	fs := flag.NewFlagSet("images remove", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "remove what it lists")
	bases := fs.Bool("bases", false, "the pinned bases too")
	rest, code, ok := parseArgs(env, fs, args, imagesUsage)
	if !ok {
		return code
	}
	pins, ok := imagePins(env, "remove", rest)
	if !ok {
		return ExitUsage
	}
	if len(rest) == 0 {
		pins = container.Pins()
	}
	layout, err := home.Resolve(env.Getenv)
	if err != nil {
		return fail(env, err)
	}
	d, err := env.docker(ctx)
	if err != nil {
		return fail(env, fmt.Errorf("the images need Docker: %w", err))
	}
	defer d.Close()
	// With --yes, the inventory, the record, the removals and the record's save happen under the lock images pull
	// holds, so a pull meanwhile is never lost from the record. A dry run writes nothing, not even the lock.
	if *yes && layoutExists(layout) {
		unlock, err := home.LockFile(ctx, builtImagesPath(layout)+".lock", func() {
			fmt.Fprintln(env.Stdout, "waiting for agentium images pull to finish")
		})
		if err != nil {
			return fail(env, err)
		}
		defer unlock()
	}
	targets, err := removalTargets(ctx, d, pins, *bases)
	if err != nil {
		return fail(env, err)
	}
	if len(targets) == 0 {
		fmt.Fprintln(env.Stdout, "No such image is on this machine's Docker.")
		return ExitOK
	}
	if !*yes {
		printRemoval(env, targets, *bases)
		return ExitOK
	}
	return removeImages(ctx, env, d, layout, targets)
}

// removalTargets are the images of the named pins on the daemon: the grading images, and with bases the pinned bases.
func removalTargets(ctx context.Context, d DockerClient, pins []container.Pin, bases bool) ([]container.LocalImage, error) {
	local, err := d.LocalImages(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range pins {
		names = append(names, p.Name())
	}
	var targets []container.LocalImage
	for _, l := range local {
		if slices.Contains(names, l.Toolchain) && (l.Kind == "grading" || bases) {
			targets = append(targets, l)
		}
	}
	return targets, nil
}

// freed is what removing an image frees: nothing when it stays under a tag of yours (its name goes, not the image).
func freed(l container.LocalImage) int64 {
	if len(l.OtherTags) > 0 {
		return 0
	}
	return l.Size
}

// imageLine is an image of the removal list.
func imageLine(l container.LocalImage) string {
	if len(l.OtherTags) > 0 {
		return fmt.Sprintf("%-8s %-12s %s (stays as %s, your own tag: frees nothing)", l.Kind, l.Toolchain, l.Ref, strings.Join(l.OtherTags, ", "))
	}
	return fmt.Sprintf("%-8s %-12s %s (%s)", l.Kind, l.Toolchain, l.Ref, formatBytes(l.Size))
}

func printRemoval(env Env, targets []container.LocalImage, bases bool) {
	st := env.style()
	var total int64
	for _, l := range targets {
		total += freed(l)
	}
	fmt.Fprintln(env.Stdout, st.Heading(fmt.Sprintf("Removing these would free up to %s (a dry run: nothing was removed):", formatBytes(total))))
	for _, l := range targets {
		fmt.Fprintln(env.Stdout, "  "+imageLine(l))
	}
	fmt.Fprintf(env.Stdout, "\nRun %s to remove them; a later container grade then needs agentium images pull again.\n", st.Command("agentium images remove --yes"+map[bool]string{true: " --bases", false: ""}[bases]))
}

// removeImages removes the targets (the caller holds the record's lock) and drops them from the record.
func removeImages(ctx context.Context, env Env, d DockerClient, layout home.Layout, targets []container.LocalImage) int {
	built, err := container.LoadBuilt(builtImagesPath(layout))
	if err != nil {
		return fail(env, err)
	}
	failed := 0
	for _, l := range targets {
		if err := d.RemoveImage(ctx, l.Ref); err != nil {
			fmt.Fprintln(env.Stdout, warning(env.style(), err.Error()))
			failed++
			continue
		}
		delete(built, l.Ref)
		if len(l.OtherTags) > 0 {
			fmt.Fprintf(env.Stdout, "untagged %s %s: it stays as %s, your own tag\n", l.Kind, l.Ref, strings.Join(l.OtherTags, ", "))
		} else {
			fmt.Fprintf(env.Stdout, "removed %s %s (%s)\n", l.Kind, l.Ref, formatBytes(l.Size))
		}
	}
	if layoutExists(layout) {
		if err := built.Save(builtImagesPath(layout)); err != nil {
			return fail(env, err)
		}
	}
	if failed > 0 {
		return ExitError
	}
	return ExitOK
}

// layoutExists reports whether the data folder exists (remove writes its record only then).
func layoutExists(layout home.Layout) bool {
	info, err := os.Stat(layout.Root)
	return err == nil && info.IsDir()
}
