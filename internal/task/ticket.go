package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Ticket formats.
const (
	TicketJira     = "jira"     // Jira's JSON export of one issue
	TicketMarkdown = "markdown" // a title, a description and an "Acceptance criteria" section
)

// Ticket is an exported issue as plain text. Key is the issue key (for example ABC-123), or "" when the ticket has
// none. Acceptance holds one entry per criterion; a nested criterion keeps two spaces of indentation per level.
type Ticket struct {
	Format      string
	Key         string
	Title       string
	Description string
	Acceptance  []string
}

// ParseTicket reads an exported ticket. The format comes from the file name's extension (.json is Jira; .md, .markdown
// and .txt are Markdown), else from the content (a JSON object or array is Jira). It never contacts Jira.
func ParseTicket(name string, data []byte) (Ticket, error) {
	var t Ticket
	var err error
	switch format := ticketFormat(name, data); format {
	case TicketJira:
		t, err = parseJira(data)
	default:
		t, err = parseMarkdown(string(data))
	}
	if err != nil {
		return Ticket{}, err
	}
	if t.Title == "" {
		return Ticket{}, fmt.Errorf("the %s ticket has no title", t.Format)
	}
	return t, nil
}

func ticketFormat(name string, data []byte) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".json":
		return TicketJira
	case ".md", ".markdown", ".txt":
		return TicketMarkdown
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid(trimmed) {
		return TicketJira
	}
	return TicketMarkdown
}

// Instruction is the task instruction the ticket gives: the title, a blank line, the description, then
// "Acceptance criteria:" with one "- " line per criterion. The key is not part of it (the task keeps it).
func (t Ticket) Instruction() string {
	parts := []string{t.Title}
	if t.Description != "" {
		parts = append(parts, t.Description)
	}
	if len(t.Acceptance) > 0 {
		lines := []string{"Acceptance criteria:"}
		for _, c := range t.Acceptance {
			trimmed := strings.TrimLeft(c, " ")
			lines = append(lines, c[:len(c)-len(trimmed)]+"- "+trimmed)
		}
		parts = append(parts, strings.Join(lines, "\n"))
	}
	return strings.Join(parts, "\n\n")
}

// ticketKey matches a Jira issue key.
var ticketKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)

// keyedTitle matches a title that starts with a key: "ABC-123: Title", "[ABC-123] Title" or "ABC-123 - Title".
var keyedTitle = regexp.MustCompile(`^\[?([A-Z][A-Z0-9_]*-[0-9]+)\]?\s*(?:[:\-–—]\s*)?(.+)$`)

// jiraIssue is the part of a Jira issue export Agentium reads. Names is present when the export expanded field names
// (?expand=names); it is how a custom acceptance-criteria field is found.
type jiraIssue struct {
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
	Names  map[string]string          `json:"names"`
}

func parseJira(data []byte) (Ticket, error) {
	issue, err := oneJiraIssue(data)
	if err != nil {
		return Ticket{}, err
	}
	t := Ticket{Format: TicketJira}
	if key := strings.TrimSpace(issue.Key); ticketKey.MatchString(key) {
		t.Key = key
	}
	var summary string
	if raw, ok := issue.Fields["summary"]; !ok || json.Unmarshal(raw, &summary) != nil {
		return Ticket{}, errors.New("the Jira export has no fields.summary (export one issue as JSON)")
	}
	t.Title = oneLine(stripHTML(summary))
	description, err := jiraText(issue.Fields["description"])
	if err != nil {
		return Ticket{}, fmt.Errorf("Jira description: %w", err)
	}
	if field := acceptanceField(issue); field != "" {
		criteria, err := jiraText(issue.Fields[field])
		if err != nil {
			return Ticket{}, fmt.Errorf("Jira %s: %w", field, err)
		}
		t.Acceptance = fieldCriteria(criteria)
	}
	// Teams often keep the criteria in the description instead, or as well: its section joins the field's list.
	description, inDescription, _ := splitAcceptance(description)
	for _, c := range inDescription {
		if !slices.Contains(t.Acceptance, c) {
			t.Acceptance = append(t.Acceptance, c)
		}
	}
	t.Description = plainHeadings(description)
	return t, nil
}

// fieldCriteria reads an acceptance-criteria field's text. A field that repeats its own heading ("h3. Acceptance
// criteria") gives that section's criteria; otherwise every line but headings counts.
func fieldCriteria(text string) []string {
	if _, criteria, found := splitAcceptance(text); found {
		return criteria
	}
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if !mdHeading.MatchString(l) {
			lines = append(lines, l)
		}
	}
	return criteriaItems(lines)
}

// oneJiraIssue finds the single issue in an export: an issue object, a search result {"issues": [...]} or an array.
func oneJiraIssue(data []byte) (jiraIssue, error) {
	var issues []jiraIssue
	var probe struct {
		Issues json.RawMessage `json:"issues"`
		Fields json.RawMessage `json:"fields"`
	}
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &issues); err != nil {
			return jiraIssue{}, fmt.Errorf("read the Jira export: %w", err)
		}
	case json.Unmarshal(trimmed, &probe) != nil:
		return jiraIssue{}, fmt.Errorf("read the Jira export: %w", json.Unmarshal(trimmed, &probe))
	case probe.Fields == nil && probe.Issues != nil:
		if err := json.Unmarshal(probe.Issues, &issues); err != nil {
			return jiraIssue{}, fmt.Errorf("read the Jira export's issues: %w", err)
		}
	default:
		var issue jiraIssue
		if err := json.Unmarshal(trimmed, &issue); err != nil {
			return jiraIssue{}, fmt.Errorf("read the Jira export: %w", err)
		}
		issues = []jiraIssue{issue}
	}
	if len(issues) != 1 {
		return jiraIssue{}, fmt.Errorf("the Jira export holds %d issues; export exactly one", len(issues))
	}
	if issues[0].Fields == nil {
		return jiraIssue{}, errors.New("the Jira export has no fields (export one issue as JSON)")
	}
	return issues[0], nil
}

// acceptanceField names the issue's acceptance-criteria field: one whose display name (from the export's names) or
// whose own name, without case, spaces and punctuation, reads "acceptance criteria". It is "" when there is none.
func acceptanceField(issue jiraIssue) string {
	isCriteria := func(s string) bool {
		s = strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' {
				return r
			}
			return -1
		}, strings.ToLower(s))
		return s == "acceptancecriteria" || s == "acceptancecriterion"
	}
	var found []string
	for id := range issue.Fields {
		if isCriteria(id) || isCriteria(issue.Names[id]) {
			found = append(found, id)
		}
	}
	if len(found) == 0 {
		return ""
	}
	return slices.Min(found) // a stable choice when several fields qualify
}

// jiraText converts a Jira text field to plain text: null is "", a string is wiki markup (see wikiToText: plain text
// stays as it is, Markdown does not), an object is Atlassian Document Format, and an array of strings is a list.
func jiraText(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", err
		}
		return wikiToText(s), nil
	case '{':
		var doc adfNode
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return "", fmt.Errorf("read Atlassian Document Format: %w", err)
		}
		return adfToText(doc), nil
	case '[':
		var items []string
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return "", fmt.Errorf("expected text, a document or a list of strings: %w", err)
		}
		lines := make([]string, len(items))
		for i, item := range items {
			lines[i] = "- " + oneLine(wikiToText(item))
		}
		return strings.Join(lines, "\n"), nil
	}
	return "", fmt.Errorf("expected text or a document, got %.20s", trimmed)
}

func parseMarkdown(text string) (Ticket, error) {
	t := Ticket{Format: TicketMarkdown}
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\uFEFF"), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	lines, t.Key = frontMatter(lines)
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i == len(lines) {
		return Ticket{}, errors.New("the Markdown ticket is empty")
	}
	title := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[i]), "#"))
	if m := keyedTitle.FindStringSubmatch(title); m != nil {
		if t.Key == "" {
			t.Key = m[1]
		}
		title = m[2]
	}
	t.Title = oneLine(stripHTML(title))
	body := lines[i+1:]
	if j := firstContent(body); j < len(body) && headingText(body[j]) == "description" {
		body = body[j+1:] // the layout already says it is the description
	}
	description, criteria, _ := splitAcceptance(strings.Join(body, "\n"))
	t.Description, t.Acceptance = stripHTMLOutsideCode(description), criteria
	return t, nil
}

// frontMatter drops a YAML front matter block (--- ... ---) and returns its key, jira or ticket entry, if any.
func frontMatter(lines []string) ([]string, string) {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return lines, ""
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			var key string
			for _, l := range lines[1:i] {
				name, value, ok := strings.Cut(l, ":")
				value = strings.Trim(strings.TrimSpace(value), `"'`)
				switch strings.ToLower(strings.TrimSpace(name)) {
				case "key", "jira", "ticket", "issue":
					if ok && ticketKey.MatchString(value) {
						key = value
					}
				}
			}
			return lines[i+1:], key
		}
	}
	return lines, ""
}

func firstContent(lines []string) int {
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	return i
}

var (
	mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	// listItem matches a Markdown list item: indentation, a marker (-, *, + or a number) and an optional task box.
	listItem = regexp.MustCompile(`^(\s*)(?:[-*+]|[0-9]+[.)])\s+(?:\[[ xX]\]\s+)?(.*)$`)
	fence    = regexp.MustCompile("^\\s*(```|~~~)")
)

// headingText is a line's text, lowercased and without heading marks, emphasis and a final colon, when the line could
// be a heading ("## Acceptance criteria", "**Acceptance Criteria:**", "Acceptance criteria:"); else "".
func headingText(line string) string {
	s := strings.TrimSpace(line)
	if m := mdHeading.FindStringSubmatch(s); m != nil {
		s = m[2]
	}
	s = strings.Trim(s, "*_ ")
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ":"))
	s = strings.Trim(s, "*_ ")
	if len(s) > 40 {
		return ""
	}
	return strings.ToLower(s)
}

// SolutionSections lists the instruction's lines that look like headings of sections that may give the solution away
// ("Solution", "Proposed fix", "Root cause" and the like), as written.
func SolutionSections(instruction string) []string {
	var found []string
	for _, l := range strings.Split(instruction, "\n") {
		if strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "\t") {
			continue
		}
		switch headingText(l) {
		case "solution", "proposed solution", "suggested solution", "possible solution", "fix", "proposed fix", "suggested fix",
			"possible fix", "root cause", "cause", "workaround", "implementation", "implementation notes", "technical notes", "how to fix":
			found = append(found, strings.TrimSpace(l))
		}
	}
	return found
}

func isAcceptanceHeading(line string) bool {
	switch headingText(line) {
	case "acceptance criteria", "acceptance criterion", "acceptance":
		return true
	}
	return false
}

// splitAcceptance takes the "Acceptance criteria" section out of Markdown-like text. A Markdown heading's section ends
// at the next heading of the same or a higher level; a bold or plain "Acceptance criteria:" line's at the next
// heading of any level. Lines in fenced or indented code never start or end it. found reports whether there was such
// a heading.
func splitAcceptance(text string) (description string, criteria []string, found bool) {
	lines := strings.Split(text, "\n")
	start, level := -1, 7
	inFence := false
	for i, l := range lines {
		if fence.MatchString(l) {
			inFence = !inFence
			continue
		}
		if inFence || strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "\t") {
			continue
		}
		if start < 0 {
			if isAcceptanceHeading(l) {
				start = i
				if m := mdHeading.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
					level = len(m[1])
				}
			}
			continue
		}
		if m := mdHeading.FindStringSubmatch(strings.TrimSpace(l)); m != nil && len(m[1]) <= level {
			criteria = criteriaItems(lines[start+1 : i])
			rest := append(append([]string{}, lines[:start]...), lines[i:]...)
			return tidy(strings.Join(rest, "\n")), criteria, true
		}
	}
	if start < 0 {
		return tidy(text), nil, false
	}
	return tidy(strings.Join(lines[:start], "\n")), criteriaItems(lines[start+1:]), true
}

// criteriaItems turns a section's lines into criteria: its list items (continuation lines joined on), or, when it has
// no list, its paragraphs.
func criteriaItems(lines []string) []string {
	hasList := false
	for _, l := range lines {
		if listItem.MatchString(l) {
			hasList = true
			break
		}
	}
	var items []string
	var current []string
	flush := func() {
		if len(current) > 0 {
			items = append(items, strings.Join(current, " "))
			current = nil
		}
	}
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		switch m := listItem.FindStringSubmatch(l); {
		case trimmed == "":
			if !hasList {
				flush()
			}
		case hasList && m != nil:
			flush()
			indent := len(strings.ReplaceAll(m[1], "\t", "    ")) / 2
			current = []string{strings.Repeat("  ", min(indent, 4)) + strings.TrimSpace(stripHTML(m[2]))}
		default:
			if t := strings.TrimSpace(stripHTML(trimmed)); t != "" {
				current = append(current, t)
			}
		}
	}
	flush()
	return items
}

// tidy trims trailing spaces, drops leading and trailing blank lines and keeps at most one blank line in a row.
func tidy(text string) string {
	var out []string
	blank := false
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank = len(out) > 0
			continue
		}
		if blank {
			out = append(out, "")
			blank = false
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// plainHeadings turns Markdown heading lines outside code into their text (Jira text is converted through Markdown-like
// headings so its acceptance criteria can be found, but is shown as plain text).
func plainHeadings(text string) string {
	lines := strings.Split(text, "\n")
	inFence := false
	for i, l := range lines {
		if fence.MatchString(l) {
			inFence = !inFence
			continue
		}
		if m := mdHeading.FindStringSubmatch(l); m != nil && !inFence {
			lines[i] = m[2]
		}
	}
	return strings.Join(lines, "\n")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlBreak   = regexp.MustCompile(`(?i)<br\s*/?>\n?`)
	htmlBlock   = regexp.MustCompile(`(?i)</(p|div|h[1-6]|tr|li|ul|ol|pre|blockquote|table)\s*>|<(hr)(\s[^<>]*)?/?>`)
	htmlItem    = regexp.MustCompile(`(?i)<li(\s[^<>]*)?>`)
	htmlLink    = regexp.MustCompile(`(?is)<a\s[^<>]*href\s*=\s*["']([^"']*)["'][^<>]*>(.*?)</a\s*>`)
	// htmlTag matches known HTML tags only, so text like List<String> or a <placeholder> survives.
	htmlTag = regexp.MustCompile(`(?i)</?(p|br|div|span|b|i|u|s|em|strong|del|ins|sub|sup|small|big|font|tt|code|pre|a|ul|ol|li|h[1-6]|` +
		`table|thead|tbody|tfoot|tr|td|th|img|hr|blockquote|details|summary|kbd|mark|section|article|header|footer|figure|caption|center)(\s[^<>]*)?/?>`)
	blankRun = regexp.MustCompile(`\n{3,}`)
)

// stripHTML removes HTML tags (known tag names only) and comments, keeps line breaks and list items, writes links as
// "text (url)" and decodes entities. Text without tags or entities is returned as is.
func stripHTML(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return s
	}
	s = htmlComment.ReplaceAllString(s, "")
	s = htmlLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := htmlLink.FindStringSubmatch(m)
		text := strings.TrimSpace(htmlTag.ReplaceAllString(sub[2], ""))
		switch {
		case text == "" || text == sub[1]:
			return sub[1]
		case sub[1] == "" || strings.HasPrefix(sub[1], "#"):
			return text
		}
		return text + " (" + sub[1] + ")"
	})
	s = htmlBreak.ReplaceAllString(s, "\n")
	s = htmlBlock.ReplaceAllString(s, "\n")
	s = htmlItem.ReplaceAllString(s, "\n- ")
	s = htmlTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return blankRun.ReplaceAllString(s, "\n\n")
}

// stripHTMLOutsideCode strips HTML from Markdown, leaving fenced code blocks alone, and tidies the result.
func stripHTMLOutsideCode(text string) string {
	var out, prose []string
	flush := func() {
		if len(prose) > 0 {
			out = append(out, stripHTML(strings.Join(prose, "\n")))
			prose = nil
		}
	}
	inFence := false
	for _, l := range strings.Split(text, "\n") {
		if fence.MatchString(l) {
			if !inFence {
				flush()
			}
			inFence = !inFence
			out = append(out, l)
			continue
		}
		if inFence {
			out = append(out, l)
		} else {
			prose = append(prose, l)
		}
	}
	flush()
	return tidy(strings.Join(out, "\n"))
}
