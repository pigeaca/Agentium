package task

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateTickets = flag.Bool("update-tickets", false, "rewrite testdata/tickets/*.want from the parsers")

// Each fixture's instruction and key match its .want file: the key on the first line, then the instruction.
func TestParseTicketFixtures(t *testing.T) {
	for _, name := range []string{"jira-plain.json", "jira-adf.json", "jira-wiki.json", "ticket.md"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "tickets", name))
			if err != nil {
				t.Fatal(err)
			}
			ticket, err := ParseTicket(name, data)
			if err != nil {
				t.Fatal(err)
			}
			got := "key: " + ticket.Key + "\nformat: " + ticket.Format + "\n\n" + ticket.Instruction() + "\n"
			want := filepath.Join("testdata", "tickets", strings.TrimSuffix(name, filepath.Ext(name))+".want")
			if *updateTickets {
				if err := os.WriteFile(want, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			expected, err := os.ReadFile(want)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(expected) {
				t.Errorf("got:\n%s\nwant:\n%s", got, expected)
			}
		})
	}
}

func TestTicketFormatDetection(t *testing.T) {
	jira := []byte(`{"key":"A-1","fields":{"summary":"Fix it"}}`)
	for _, c := range []struct {
		name string
		data []byte
		want string
	}{
		{"issue.json", jira, TicketJira},
		{"ISSUE.JSON", []byte("not json"), TicketJira},
		{"issue.md", jira, TicketMarkdown},
		{"issue.txt", []byte("# Title"), TicketMarkdown},
		{"export", jira, TicketJira},
		{"export", []byte(`[{"key":"A-1","fields":{"summary":"x"}}]`), TicketJira},
		{"export", []byte("{ not json"), TicketMarkdown},
		{"export", []byte("# Title\n"), TicketMarkdown},
	} {
		if got := ticketFormat(c.name, c.data); got != c.want {
			t.Errorf("ticketFormat(%q, %.20q) = %s, want %s", c.name, c.data, got, c.want)
		}
	}
}

func TestParseTicketErrors(t *testing.T) {
	for name, c := range map[string]struct {
		file, data, want string
	}{
		"two issues":  {"x.json", `{"issues":[{"fields":{"summary":"a"}},{"fields":{"summary":"b"}}]}`, "holds 2 issues"},
		"no issues":   {"x.json", `{"issues":[]}`, "holds 0 issues"},
		"no summary":  {"x.json", `{"key":"A-1","fields":{"description":"d"}}`, "no fields.summary"},
		"no fields":   {"x.json", `{"key":"A-1"}`, "no fields"},
		"not json":    {"x.json", `nope`, "not a JSON object"},
		"bad field":   {"x.json", `{"fields":{"summary":"a","description":42}}`, "Jira description"},
		"empty title": {"x.json", `{"fields":{"summary":"  "}}`, "has no title"},
		"empty md":    {"x.md", "\n\n  \n", "is empty"},
	} {
		if _, err := ParseTicket(c.file, []byte(c.data)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestParseTicketVariants(t *testing.T) {
	// A Jira field named acceptance criteria comes first, then the description's own section (without repeats), and a
	// key that is not an issue key is dropped.
	tk, err := ParseTicket("x.json", []byte(`{"key":"not a key","fields":{"summary":"Title","description":"Body\n\nAcceptance criteria:\n* also *this*\n* second",`+
		`"acceptance_criteria":["first *one*","second"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Title\n\nBody\n\nAcceptance criteria:\n- first one\n- second\n- also this"; tk.Instruction() != want || tk.Key != "" {
		t.Errorf("got key %q, instruction:\n%s\nwant:\n%s", tk.Key, tk.Instruction(), want)
	}
	// A Markdown ticket with only a title, its key in the title, and plain paragraphs as criteria.
	tk, err = ParseTicket("x.md", []byte("[ABC-12] Make it fast\n"))
	if err != nil || tk.Key != "ABC-12" || tk.Instruction() != "Make it fast" {
		t.Errorf("title only: %+v, %v", tk, err)
	}
	tk, err = ParseTicket("x.md", []byte("ABC-12: Make it fast\nIt is slow.\n\n**Acceptance Criteria:**\nUnder a second\nfor 1000 rows.\n\nNo new dependencies.\n"))
	if err != nil || tk.Key != "ABC-12" || tk.Description != "It is slow." ||
		strings.Join(tk.Acceptance, "|") != "Under a second for 1000 rows.|No new dependencies." {
		t.Errorf("paragraph criteria: %+v, %v", tk, err)
	}
	// An "Acceptance criteria" heading inside a code block is code.
	tk, err = ParseTicket("x.md", []byte("# T\n\n```\n## Acceptance criteria\n```\n"))
	if err != nil || len(tk.Acceptance) != 0 || !strings.Contains(tk.Description, "## Acceptance criteria") {
		t.Errorf("code block: %+v, %v", tk, err)
	}
}

func TestWikiInline(t *testing.T) {
	for in, want := range map[string]string{
		"*bold* and *more*":              "bold and more",
		"_it_ snake_case_name a_b":       "it snake_case_name a_b",
		"a * b * c, C++ and C++":         "a * b * c, C++ and C++",
		"+under+ ??cite??":               "under cite",
		"{{code}} [label|http://x.y]":    "code label (http://x.y)",
		"[http://x.y|http://x.y] [~bob]": "http://x.y @bob",
		"[^log.txt] !a.png!":             "log.txt ",
		"line\\\\next":                   "line\nnext",
	} {
		if got := wikiInline(in); got != want {
			t.Errorf("wikiInline(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripHTML(t *testing.T) {
	for in, want := range map[string]string{
		"plain List<String> & co":                   "plain List<String> & co",
		"<p>One</p><p>Two &amp; <em>three</em></p>": "One\nTwo & three\n",
		`<a href="https://x.y">docs</a> <!-- c -->`: "docs (https://x.y) ",
		"<ul><li>a</li><li>b</li></ul>":             "\n- a\n\n- b\n\n",
		"keep <placeholder> and <T>":                "keep <placeholder> and <T>",
		"a<br>b<BR/>\nc":                            "a\nb\nc",
	} {
		if got := stripHTML(in); got != want {
			t.Errorf("stripHTML(%q) = %q, want %q", in, got, want)
		}
	}
}
