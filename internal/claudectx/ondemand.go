package claudectx

import (
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pigeaca/agentium/internal/source"
)

// RuleGlobs returns a scoped rule's paths: patterns, from its frontmatter: one pattern, an inline list ([a, b]) or a
// block list (- a). Claude Code loads the rule when the agent works with a file a pattern matches.
func RuleGlobs(data []byte) []string {
	lines := frontmatter(data)
	var globs []string
	for i, line := range lines {
		value, ok := strings.CutPrefix(line, "paths:")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]"):
			for _, item := range strings.Split(strings.Trim(value, "[]"), ",") {
				globs = appendGlob(globs, item)
			}
		case value != "":
			globs = appendGlob(globs, value)
		default:
			for _, next := range lines[i+1:] {
				item, ok := strings.CutPrefix(strings.TrimSpace(next), "- ")
				if !ok {
					break
				}
				globs = appendGlob(globs, item)
			}
		}
		break
	}
	return globs
}

func appendGlob(globs []string, item string) []string {
	if g := strings.Trim(strings.TrimSpace(item), `"'`); g != "" {
		return append(globs, g)
	}
	return globs
}

// GlobMatch reports whether a repository-relative path matches a rule pattern: * matches within a folder, ** across
// folders, ? one character, {a,b} either choice. A pattern without a slash also matches a file's name in any folder.
// This follows the common glob rules; Claude Code's own matcher may differ at the edges.
func GlobMatch(pattern, p string) bool {
	re, err := regexp.Compile("^" + globPattern(pattern) + "$")
	if err != nil {
		return false
	}
	if re.MatchString(p) {
		return true
	}
	return !strings.Contains(pattern, "/") && re.MatchString(path.Base(p))
}

// globPattern turns a glob into a regular expression (without anchors).
func globPattern(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '{':
			end := strings.IndexByte(glob[i:], '}')
			if end < 0 {
				b.WriteString(regexp.QuoteMeta(glob[i:]))
				return b.String()
			}
			var choices []string
			for _, choice := range strings.Split(glob[i+1:i+end], ",") {
				choices = append(choices, globPattern(choice))
			}
			b.WriteString("(?:" + strings.Join(choices, "|") + ")")
			i += end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// SubagentNames lists a context's project subagents: each file's name without .md, and the name its frontmatter gives.
func SubagentNames(c Context, src source.Source) []string {
	var names []string
	for _, e := range c.Entries {
		if e.Kind != KindSubagent {
			continue
		}
		names = append(names, strings.TrimSuffix(path.Base(e.Path), ".md"))
		if data, err := src.ReadFile(e.Path); err == nil {
			if name := frontmatterField(data, "name"); name != "" {
				names = append(names, strings.Trim(name, `"'`))
			}
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}
