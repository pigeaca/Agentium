package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pigeaca/agentium/internal/report"
)

// The contract golden (testdata/contract.golden) lists what users script against, generated from the real code:
// commands, every flag's name, type and default, the exit codes, the --json document schemas, the store migrations and
// the experiment design and method versions. `harness.py release plan` diffs it between the last tag and HEAD: a
// removed or changed line is a breaking change, an added line an additive one. An internal rename or a moved file
// changes nothing here. Regenerate with `go test ./internal/cli -run TestContractSurface -update-contract`.
var updateContract = flag.Bool("update-contract", false, "rewrite testdata/contract.golden")

// jsonDocs names the document types each command's --json output is made of; TestEveryJSONDocumentIsListed fails when
// a document type embedding `header` is missing from it.
var jsonDocs = map[string][]any{
	"init":                 {initDoc{}},
	"start":                {startDoc{}},
	"clean":                {cleanDoc{}},
	"context show":         {contextShowDoc{}},
	"context snapshot":     {snapshotDoc{}},
	"context list":         {snapshotListDoc{}},
	"context diff":         {diffDoc{}},
	"context lint":         {lintDoc{}},
	"task list":            {taskListDoc{}},
	"task show":            {taskShowDoc{}},
	"task validate":        {validatedDoc{}, validateAllDoc{}},
	"task add":             {taskSavedDoc{}},
	"task import":          {taskSavedDoc{}},
	"task edit":            {taskEditDoc{}},
	"task rm":              {taskRemovedDoc{}},
	"run once":             {runOnceDoc{}},
	"run show":             {runShowDoc{}},
	"run list":             {runListDoc{}},
	"experiment new":       {experimentNewDoc{}},
	"experiment plan":      {experimentPlanDoc{}},
	"experiment show":      {experimentShowDoc{}},
	"experiment list":      {experimentListDoc{}},
	"experiment run":       {experimentRunDoc{}},
	"experiment rm":        {experimentRemoveDoc{}},
	"experiment report":    {report.Report{}},
	"pool update":          {poolUpdateDoc{}},
	"pool status":          {poolStatusDoc{}},
	"(any failed command)": {errorDoc{}},
}

// parsedPackage parses the non-test Go files of a package folder.
func parsedPackage(t *testing.T, dir string) []*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(info os.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}
	return files
}

// flagSetNames are the names every flag.NewFlagSet call in the package gives its set: each is a command.
func flagSetNames(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range parsedPackage(t, ".") {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewFlagSet" {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok {
					name, _ := strconv.Unquote(lit.Value)
					seen[name] = true
				}
			}
			return true
		})
	}
	var names []string
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// constants lists the named constants of a package whose names match, with their literal or identifier values.
func constants(t *testing.T, dir string, match *regexp.Regexp) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range parsedPackage(t, dir) {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !match.MatchString(name.Name) || i >= len(vs.Values) {
						continue
					}
					switch v := vs.Values[i].(type) {
					case *ast.BasicLit:
						out[name.Name] = v.Value
					case *ast.Ident:
						out[name.Name] = v.Name
					}
				}
			}
		}
	}
	return out
}

// schema lists the JSON key paths and kinds of a type as encoding/json would write them (embedded structs without a
// tag are flattened; "-" fields are skipped).
func schema(prefix string, t reflect.Type, depth int, out map[string]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if depth > 8 || t.PkgPath() == "time" {
			if t.PkgPath() == "time" {
				out[prefix] = "time"
			}
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag, hasTag := f.Tag.Lookup("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" || (!f.IsExported() && !f.Anonymous) {
				continue
			}
			if f.Anonymous && name == "" {
				schema(prefix, f.Type, depth+1, out)
				continue
			}
			if !hasTag || name == "" {
				name = f.Name
			}
			path := prefix + "." + name
			kind := f.Type
			for kind.Kind() == reflect.Pointer {
				kind = kind.Elem()
			}
			out[path] = kindName(kind) + map[bool]string{true: "?", false: ""}[strings.Contains(opts, "omitempty")]
			schema(path, kind, depth+1, out)
		}
	case reflect.Slice, reflect.Array:
		schema(prefix+"[]", t.Elem(), depth+1, out)
	case reflect.Map:
		schema(prefix+"{}", t.Elem(), depth+1, out)
	}
}

func kindName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Struct:
		if t.PkgPath() == "time" {
			return "time"
		}
		return "object"
	case reflect.Slice, reflect.Array:
		return "[]" + kindName(t.Elem())
	case reflect.Map:
		return "map"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	}
	return t.Kind().String()
}

// contractLines is the golden's content: "key" or "key<TAB>value" lines, sorted.
func contractLines(t *testing.T) []string {
	t.Helper()
	var lines []string
	add := func(key, value string) { // no trailing tab for an empty value: the commit hook rejects trailing whitespace
		if value != "" {
			key += "\t" + value
		}
		lines = append(lines, key)
	}

	// Commands and flags, from the real flag sets: each command is run with -h, and parseArgs hands over its set.
	for _, name := range flagSetNames(t) {
		var found *flag.FlagSet
		env := Env{Args: append(strings.Fields(name), "-h"), Stdout: io.Discard, Stderr: io.Discard, DefaultGrader: "host", Dir: t.TempDir(),
			Getenv: func(string) string { return "" }, Environ: func() []string { return nil },
			flagSink: func(fs *flag.FlagSet) {
				if fs.Name() == name {
					found = fs
				}
			}}
		Run(context.Background(), env)
		if found == nil {
			t.Fatalf("command %q built no flag set when run with -h: the contract test cannot list its flags", name)
		}
		add("command "+name, "")
		found.VisitAll(func(f *flag.Flag) {
			add(fmt.Sprintf("flag %s --%s", name, f.Name), fmt.Sprintf("%s default=%q", flagKind(f.Value), f.DefValue))
		})
	}
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z][a-z-]*) {2,}\S`).FindAllStringSubmatch(usage, -1) {
		add("command "+m[1], "")
	}
	for command, subs := range jsonCommands {
		for _, sub := range subs {
			add("json-flag "+strings.TrimSpace(command+" "+sub), "")
		}
	}

	for name, value := range constants(t, ".", regexp.MustCompile(`^Exit[A-Z]\w*$`)) {
		add("exit "+name, value)
	}
	for name, value := range constants(t, "../experiment", regexp.MustCompile(`^(Design|Method)\w*$`)) {
		add("experiment "+name, value)
	}

	for command, docs := range jsonDocs {
		for _, doc := range docs {
			keys := map[string]string{}
			schema("", reflect.TypeOf(doc), 0, keys)
			for path, kind := range keys {
				add(fmt.Sprintf("json %s %s", command, path), kind)
			}
		}
	}
	add("json JSONSchema", strconv.Itoa(JSONSchema))

	migrations, err := filepath.Glob("../store/migrations/*")
	if err != nil || len(migrations) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	for _, path := range migrations {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		add("migration "+filepath.Base(path), hex.EncodeToString(sum[:8]))
	}
	sort.Strings(lines)
	return slicesCompact(lines)
}

// flagKind is how a flag takes its value, never the Go type that implements it: renaming an internal flag type must
// not change the golden. "bool" appears without a value, "list" repeats, "value" takes one value.
func flagKind(v flag.Value) string {
	if b, ok := v.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return "bool"
	}
	if _, ok := v.(*stringList); ok {
		return "list"
	}
	return "value"
}

func slicesCompact(lines []string) []string {
	out := lines[:0]
	for i, l := range lines {
		if i == 0 || l != lines[i-1] {
			out = append(out, l)
		}
	}
	return out
}

func TestContractSurface(t *testing.T) {
	got := strings.Join(contractLines(t), "\n") + "\n"
	path := filepath.Join("testdata", "contract.golden")
	if *updateContract {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test ./internal/cli -run TestContractSurface -update-contract writes it)", err)
	}
	if !bytes.Equal(want, []byte(got)) {
		old, now := map[string]bool{}, map[string]bool{}
		for _, l := range strings.Split(string(want), "\n") {
			old[l] = true
		}
		var diff []string
		for _, l := range strings.Split(got, "\n") {
			now[l] = true
			if !old[l] {
				diff = append(diff, "+ "+l)
			}
		}
		for l := range old {
			if !now[l] {
				diff = append(diff, "- "+l)
			}
		}
		sort.Strings(diff)
		t.Fatalf("the contract changed (a removed or changed line breaks users; see .agents/rules/releases.md). Regenerate with\n"+
			"  go test ./internal/cli -run TestContractSurface -update-contract\nand declare it in the PR (title `!` and a Breaking: line, or feat/fix for an addition):\n%s", strings.Join(diff, "\n"))
	}
}

// Every document type that embeds header is a --json output and must be in jsonDocs, or its keys escape the golden.
func TestEveryJSONDocumentIsListed(t *testing.T) {
	listed := map[string]bool{}
	for _, docs := range jsonDocs {
		for _, d := range docs {
			listed[reflect.TypeOf(d).Name()] = true
		}
	}
	for _, f := range parsedPackage(t, ".") {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					if id, ok := field.Type.(*ast.Ident); ok && id.Name == "header" && len(field.Names) == 0 && !listed[ts.Name.Name] {
						t.Errorf("document type %s embeds header but is not in jsonDocs (contract_test.go)", ts.Name.Name)
					}
				}
			}
		}
	}
}

// A renamed internal type or a moved flag definition changes nothing; a flag named by a constant, and a lint key, are in.
func TestContractSurfaceSeesRealFlagsAndKeys(t *testing.T) {
	lines := strings.Join(contractLines(t), "\n")
	for _, want := range []string{"flag clean --yes\t", "command context lint\n", "json context lint .", "exit ExitUsage\t2", "migration 0001_projects.sql\t"} {
		if !strings.Contains(lines, want) {
			t.Errorf("contract lacks %q", want)
		}
	}
}

// The schema depends on the JSON a type writes, never on its Go name, so renaming a type changes no golden line.
func TestSchemaIgnoresTypeNames(t *testing.T) {
	type before struct {
		header
		Name string   `json:"name"`
		Opt  *int     `json:"opt,omitempty"`
		List []string `json:"list"`
	}
	type after struct { // the same document under another name
		header
		Name string   `json:"name"`
		Opt  *int     `json:"opt,omitempty"`
		List []string `json:"list"`
	}
	a, b := map[string]string{}, map[string]string{}
	schema("", reflect.TypeOf(before{}), 0, a)
	schema("", reflect.TypeOf(after{}), 0, b)
	if !reflect.DeepEqual(a, b) || a[".opt"] != "int?" || a[".schema"] != "int" || a[".list"] != "[]string" {
		t.Errorf("schemas differ or are wrong: %v vs %v", a, b)
	}
}

// A flag's kind comes from its behavior, so renaming the internal type behind it leaves the golden unchanged.
func TestFlagKindIgnoresTypeNames(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.Bool("b", false, "")
	fs.String("s", "", "")
	fs.Int("i", 0, "")
	var list stringList
	fs.Var(&list, "l", "")
	fs.Var(&removedFlag{name: "r"}, "r", "")
	got := map[string]string{}
	fs.VisitAll(func(f *flag.Flag) { got[f.Name] = flagKind(f.Value) })
	want := map[string]string{"b": "bool", "s": "value", "i": "value", "l": "list", "r": "bool"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flag kinds %v, want %v", got, want)
	}
	if lines := strings.Join(contractLines(t), "\n"); strings.Contains(lines, "*cli.") || strings.Contains(lines, "*flag.") {
		t.Error("the golden names a Go type")
	}
}
