package task

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// adfNode is a node of Atlassian Document Format, the JSON Jira Cloud uses for rich text.
type adfNode struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Content []adfNode      `json:"content"`
	Attrs   map[string]any `json:"attrs"`
	Marks   []struct {
		Type  string         `json:"type"`
		Attrs map[string]any `json:"attrs"`
	} `json:"marks"`
}

// adfToText renders a document as Markdown-like plain text: headings as "#" lines (see plainHeadings), lists as "- "
// and "1. " lines, code indented by four spaces, quotes with "> ", table rows with " | " between cells. Media are
// dropped.
func adfToText(doc adfNode) string {
	if doc.Type != "doc" && doc.Content == nil {
		return tidy(adfInline([]adfNode{doc}))
	}
	return tidy(adfBlocks(doc.Content))
}

// adfBlocks renders block nodes, separated by blank lines.
func adfBlocks(nodes []adfNode) string {
	var blocks []string
	for _, n := range nodes {
		if b := adfBlock(n); strings.TrimSpace(b) != "" {
			blocks = append(blocks, b)
		}
	}
	return strings.Join(blocks, "\n\n")
}

func adfBlock(n adfNode) string {
	switch n.Type {
	case "paragraph":
		return adfInline(n.Content)
	case "heading":
		level := 1
		if l, ok := n.Attrs["level"].(float64); ok && l >= 1 && l <= 6 {
			level = int(l)
		}
		return strings.Repeat("#", level) + " " + oneLine(adfInline(n.Content))
	case "bulletList", "orderedList", "taskList":
		number := 1
		if s, ok := n.Attrs["order"].(float64); ok {
			number = int(s)
		}
		var items []string
		for _, item := range n.Content {
			text := adfListItem(item)
			if strings.TrimSpace(text) == "" { // an empty item would be a bare "-" or "1."
				continue
			}
			marker := "- "
			if n.Type == "orderedList" {
				marker = strconv.Itoa(number) + ". "
				number++
			}
			items = append(items, prefixLines(text, marker, strings.Repeat(" ", len(marker))))
		}
		return strings.Join(items, "\n")
	case "codeBlock":
		return prefixLines(adfInline(n.Content), "    ", "    ")
	case "blockquote":
		return prefixLines(adfBlocks(n.Content), "> ", "> ")
	case "rule":
		return "---"
	case "table":
		var rows []string
		for _, row := range n.Content {
			var cells []string
			for _, cell := range row.Content {
				cells = append(cells, oneLine(adfBlocks(cell.Content)))
			}
			rows = append(rows, strings.Join(cells, " | "))
		}
		return strings.Join(rows, "\n")
	case "mediaSingle", "mediaGroup", "media", "mediaInline":
		return ""
	}
	if isADFInline(n.Type) {
		return adfInline([]adfNode{n})
	}
	return adfBlocks(n.Content) // panels, expands, layouts and other containers
}

// adfListItem renders a list item's blocks one per line (nested lists right under their item).
func adfListItem(item adfNode) string {
	if item.Type == "taskItem" || len(item.Content) == 0 || isADFInline(item.Content[0].Type) {
		return adfInline(item.Content)
	}
	var lines []string
	for _, b := range item.Content {
		if s := adfBlock(b); strings.TrimSpace(s) != "" {
			lines = append(lines, s)
		}
	}
	return strings.Join(lines, "\n")
}

func isADFInline(t string) bool {
	switch t {
	case "text", "hardBreak", "mention", "emoji", "inlineCard", "status", "date", "placeholder":
		return true
	}
	return false
}

func adfInline(nodes []adfNode) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case "text":
			text := n.Text
			for _, m := range n.Marks {
				if href, _ := m.Attrs["href"].(string); m.Type == "link" && href != "" && href != text {
					text += " (" + href + ")"
				}
			}
			b.WriteString(text)
		case "hardBreak":
			b.WriteString("\n")
		case "mention", "status":
			b.WriteString(attrString(n, "text"))
		case "emoji":
			if s := attrString(n, "text"); s != "" {
				b.WriteString(s)
			} else {
				b.WriteString(attrString(n, "shortName"))
			}
		case "inlineCard", "blockCard":
			b.WriteString(attrString(n, "url"))
		case "date":
			if ms, err := strconv.ParseInt(attrString(n, "timestamp"), 10, 64); err == nil {
				b.WriteString(time.UnixMilli(ms).UTC().Format("2006-01-02"))
			}
		default:
			b.WriteString(adfInline(n.Content))
		}
	}
	return b.String()
}

func attrString(n adfNode, name string) string {
	switch v := n.Attrs[name].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// prefixLines puts first before the first line of text and rest before every other non-empty line.
func prefixLines(text, first, rest string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		switch {
		case i == 0:
			lines[i] = first + l
		case l != "":
			lines[i] = rest + l
		}
	}
	return strings.Join(lines, "\n")
}

var (
	wikiCode     = regexp.MustCompile(`(?s)\{(code|noformat)(?::[^}]*)?\}(.*?)\{(?:code|noformat)\}`)
	wikiMacro    = regexp.MustCompile(`\{(quote|panel|color|section|column|expand|info|note|tip|warning)(?::[^}]*)?\}`)
	wikiHeading  = regexp.MustCompile(`^\s*h([1-6])\.\s+(.*)$`)
	wikiQuote    = regexp.MustCompile(`^\s*bq\.\s+(.*)$`)
	wikiList     = regexp.MustCompile(`^\s*([*#-]+)\s+(.*)$`)
	wikiRule     = regexp.MustCompile(`^\s*-{4,}\s*$`)
	wikiTableRow = regexp.MustCompile(`^\s*\|`)
	wikiCell     = regexp.MustCompile(`\|\|?`)
	wikiImage    = regexp.MustCompile(`!([^!\s|]+\.(?i:png|jpe?g|gif|svg|bmp|webp))(\|[^!]*)?!`)
	wikiMono     = regexp.MustCompile(`\{\{(.+?)\}\}`)
	wikiUser     = regexp.MustCompile(`\[~([^\]]+)\]`)
	wikiLink     = regexp.MustCompile(`\[([^\[\]|]+)\|([^\[\]]+)\]`)
	wikiBareLink = regexp.MustCompile(`\[((?:https?|mailto|file):[^\[\]|]+)\]`)
	wikiAttach   = regexp.MustCompile(`\[\^([^\]]+)\]`)
	// Emphasis: a mark pair around non-space text, not inside a word. The boundaries are part of the match (Go's
	// regexp has no lookaround), so each is applied twice to catch neighbours like "*a* *b*".
	wikiEmphasis = []*regexp.Regexp{
		regexp.MustCompile(`(^|[^\w*])\*(\S|\S[^*\n]*?\S)\*($|[^\w*])`),
		regexp.MustCompile(`(^|[^\w_])_(\S|\S[^_\n]*?\S)_($|[^\w_])`),
		regexp.MustCompile(`(^|[^\w+])\+(\S|\S[^+\n]*?\S)\+($|[^\w+])`),
		regexp.MustCompile(`(^|[^\w?])\?\?(\S|\S[^?\n]*?\S)\?\?($|[^\w?])`),
	}
)

// wikiToText converts basic Jira wiki markup to Markdown-like plain text (headings as "#" lines, see plainHeadings):
// emphasis and macros are dropped, links become "text (url)", lists "- " and "1. " lines, code is indented by four
// spaces, and known HTML tags are stripped. Every string is read as wiki markup, so plain text without markup stays
// as it is, but Markdown does not: \\ becomes a line break, {{x}} becomes x, and a line starting with "#" becomes a
// numbered item, so a Markdown heading in a Jira text field turns into a list item and its section is lost.
func wikiToText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out []string
	last := 0
	for _, m := range wikiCode.FindAllStringSubmatchIndex(s, -1) {
		out = append(out, wikiProse(s[last:m[0]]))
		code := strings.Trim(s[m[4]:m[5]], "\n")
		out = append(out, "\n\n"+prefixLines(code, "    ", "    ")+"\n\n")
		last = m[1]
	}
	out = append(out, wikiProse(s[last:]))
	return tidy(strings.Join(out, ""))
}

func wikiProse(s string) string {
	s = stripHTML(wikiMacro.ReplaceAllString(s, ""))
	lines := strings.Split(s, "\n")
	counters := map[int]int{} // ordered-list numbers by depth, reset by any line that is not a list item
	for i, l := range lines {
		if wikiRule.MatchString(l) {
			lines[i] = "---"
			clear(counters)
			continue
		}
		if m := wikiList.FindStringSubmatch(l); m != nil {
			depth := len(m[1])
			marker := "- "
			if strings.HasSuffix(m[1], "#") {
				counters[depth]++
				marker = fmt.Sprintf("%d. ", counters[depth])
			}
			for d := range counters {
				if d > depth {
					delete(counters, d)
				}
			}
			lines[i] = strings.Repeat("  ", depth-1) + marker + wikiInline(m[2])
			continue
		}
		clear(counters)
		if m := wikiHeading.FindStringSubmatch(l); m != nil {
			level, _ := strconv.Atoi(m[1])
			lines[i] = strings.Repeat("#", level) + " " + wikiInline(m[2])
		} else if m := wikiQuote.FindStringSubmatch(l); m != nil {
			lines[i] = "> " + wikiInline(m[1])
		} else if wikiTableRow.MatchString(l) {
			var cells []string
			for _, c := range wikiCell.Split(strings.TrimSpace(l), -1) {
				if c = strings.TrimSpace(c); c != "" {
					cells = append(cells, wikiInline(c))
				}
			}
			lines[i] = strings.Join(cells, " | ")
		} else {
			lines[i] = wikiInline(l)
		}
	}
	return strings.Join(lines, "\n")
}

func wikiInline(s string) string {
	s = strings.ReplaceAll(s, `\\`, "\n")
	s = wikiImage.ReplaceAllString(s, "")
	s = wikiMono.ReplaceAllString(s, "$1")
	s = wikiUser.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "[~accountid:") { // an opaque account ID says nothing to an agent
			return "@user"
		}
		return "@" + m[2:len(m)-1]
	})
	s = wikiLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := wikiLink.FindStringSubmatch(m)
		if strings.TrimSpace(sub[1]) == strings.TrimSpace(sub[2]) {
			return sub[2]
		}
		return sub[1] + " (" + sub[2] + ")"
	})
	s = wikiBareLink.ReplaceAllString(s, "$1")
	s = wikiAttach.ReplaceAllString(s, "$1")
	for _, re := range wikiEmphasis {
		for range 2 {
			s = re.ReplaceAllString(s, "$1$2$3")
		}
	}
	return s
}
