package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"unicode/utf8"

	"github.com/pigeaca/agentium/internal/store"
)

// gapsFormat is the layout of a GapsCache's file. A file of another one is not read; the next save replaces it.
const gapsFormat = 1

// maxCheckers is how many checkers' answers the file holds: those that saved last. One build is several checkers when
// it runs under different locale variables, as it does from a terminal, from an agent's shell and from a script.
const maxCheckers = 8

// GapsCache keeps tasks' gaps between commands, in one file of the data folder (home.Layout.GapsCache), so a list of
// tasks whose checks are kept starts no git process for them. A task's check is dozens of git searches, and its answer
// stays the same for as long as what it depends on does:
//
//   - the task's input (FairnessInput): its two commits, its instruction and its file lists. An answer is kept under a
//     digest of the input, and only when both commits are named by their full IDs (source.Pinned), which git never
//     gives another meaning;
//   - the checker: the build of Agentium that worked the answer out, and git as it runs (gitx.Identity: its build and
//     its locale decide how a search that ignores case matches). The file keeps each checker's answers apart, and a
//     checker reads only its own.
//
// So what is taken from the file is what a check would find now. Only complete answers are put there (Fairness.Gaps
// says which those are). The file holds text of hidden tests, so it lives in the data folder's cache, which no agent
// may read. It is disposable: one that cannot be read, or is of another format, counts as empty.
//
// A nil *GapsCache keeps nothing. It is safe for concurrent use.
type GapsCache struct {
	path    string
	checker string

	mu     sync.Mutex
	gaps   map[string][]Gap // by input digest: the file's, for this checker, and what this command added
	loaded int              // how many of them the file had
	added  bool
	others []checkerGaps // the file's other checkers, the one that saved last first
}

// gapsFile is the file: the answers of the checkers that saved last, the newest save first.
type gapsFile struct {
	Format   int           `json:"format"`
	Checkers []checkerGaps `json:"checkers"`
}

// checkerGaps is one checker's answers, by input digest.
type checkerGaps struct {
	Checker string           `json:"checker"`
	Gaps    map[string][]Gap `json:"gaps"`
}

// OpenGapsCache reads the file at path for the checker that build and git make: build is what tells this build of
// Agentium from any other, git is gitx.Identity. A file that is missing or unreadable, or has nothing of this checker,
// gives an empty cache; Save then says so if it cannot write one either.
func OpenGapsCache(path, build, git string) *GapsCache {
	c := &GapsCache{path: path, checker: digest(build, git), gaps: map[string][]Gap{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var file gapsFile
	if json.Unmarshal(data, &file) != nil || file.Format != gapsFormat {
		return c
	}
	for _, one := range file.Checkers {
		if one.Checker != c.checker {
			c.others = append(c.others, one)
			continue
		}
		for id, gaps := range one.Gaps {
			if len(gaps) == 0 {
				gaps = nil // as a check answers when it finds none
			}
			c.gaps[id] = gaps
		}
	}
	c.loaded = len(c.gaps)
	return c
}

// lookup returns the gaps kept for in.
func (c *GapsCache) lookup(in FairnessInput) ([]Gap, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	gaps, ok := c.gaps[digest(in.key())]
	return slices.Clone(gaps), ok
}

// store keeps gaps as in's answer; the caller has made sure the answer is complete and in's commits are pinned. Gaps
// that the file could not give back byte for byte (JSON has no way to write text that is not UTF-8) are not kept: such
// a task is checked every time.
func (c *GapsCache) store(in FairnessInput, gaps []Gap) {
	if c == nil {
		return
	}
	for _, g := range gaps {
		if !utf8.ValidString(g.Kind) || !utf8.ValidString(g.Text) || !utf8.ValidString(g.File) {
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gaps[digest(in.key())] = slices.Clone(gaps)
	c.added = true
}

// Save writes the file when this command changed what it should hold for its checker: it worked out gaps the file
// did not have, or the file has gaps of inputs that are none of tasks' (the project's tasks as they are now: an edited
// or removed task's old answer is dropped, for every checker). Otherwise the file is left untouched, so a list that
// found everything kept writes nothing. This checker's answers go first, then those of the others that saved last, as
// they were when the file was read, up to maxCheckers in all.
//
// The file is written whole beside its place and then renamed, mode 0600 in folders 0700: a reader never sees half of
// one, and two commands that save at once leave one's file, not a mix (the other's new answers are worked out again).
func (c *GapsCache) Save(tasks []store.Task) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current := map[string]bool{}
	for _, t := range tasks {
		if in, ok := inputOf(t); ok {
			current[digest(in.key())] = true
		}
	}
	mine := checkerGaps{Checker: c.checker, Gaps: map[string][]Gap{}}
	for id, gaps := range c.gaps {
		if current[id] {
			mine.Gaps[id] = append([]Gap{}, gaps...) // none is written as [], not null
		}
	}
	if !c.added && len(mine.Gaps) == c.loaded {
		return nil
	}
	file := gapsFile{Format: gapsFormat}
	for _, one := range append([]checkerGaps{mine}, c.others...) {
		for id := range one.Gaps {
			if !current[id] {
				delete(one.Gaps, id)
			}
		}
		if len(one.Gaps) > 0 && len(file.Checkers) < maxCheckers {
			file.Checkers = append(file.Checkers, one)
		}
	}
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("write %s: %w", c.path, err)
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("write %s: %w", c.path, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(c.path)+".*.tmp") // mode 0600
	if err != nil {
		return fmt.Errorf("write %s: %w", c.path, err)
	}
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %w", c.path, err)
	}
	c.added, c.loaded = false, len(mine.Gaps)
	return nil
}

// digest is the SHA-256, in hex, of texts, each written with its length, so no two lists of texts share one.
func digest(texts ...string) string {
	h := sha256.New()
	for _, text := range texts {
		fmt.Fprintf(h, "%d\x00%s", len(text), text)
	}
	return hex.EncodeToString(h.Sum(nil))
}
