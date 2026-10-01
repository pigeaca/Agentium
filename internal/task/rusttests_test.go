package task

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestRustTestItems(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want []string // the first line of each item
	}{
		"no tests":        {"fn f() {}\n", nil},
		"cfg test module": {"fn f() {}\n#[cfg(test)]\nmod tests {\n    use super::*;\n    #[test]\n    fn a() {}\n}\nfn g() {}\n", []string{"#[cfg(test)]"}},
		"test fn alone":   {"#[test]\n#[should_panic]\nfn a() { panic!() }\nfn g() {}\n", []string{"#[test]"}},
		"async test":      {"#[tokio::test]\nasync fn a() {}\n", []string{"#[tokio::test]"}},
		"all(test)":       {"#[cfg(all(test, feature = \"x\"))]\nmod m {}\n", []string{"#[cfg(all(test, feature = \"x\"))]"}},
		"not test":        {"#[cfg(not(test))]\nfn real() {}\n", nil},
		"braceless item":  {"#[cfg(test)]\nuse crate::helper;\nfn f() {}\n", []string{"#[cfg(test)]"}},
		"inner attribute": {"#![cfg(test)]\nfn a() {}\n", []string{"#![cfg(test)]"}},
		"tricky tokens": {"#[cfg(test)]\nmod tests {\n    // } in a comment\n    /* nested /* } */ } */\n" +
			"    fn a<'a>(x: &'a str) { let _ = '}'; let _ = '\\''; let _ = \"}\\\"\"; let _ = r#\"}\"#; let _ = br\"}\"; let r#fn = 1; }\n}\nfn after() {}\n",
			[]string{"#[cfg(test)]"}},
		"cfg test field":                      {"struct S {\n #[cfg(test)]\n x: i32,\n y: i32,\n}\n\nfn main() { let a = 1; }\n", []string{"#[cfg(test)]"}},
		"cfg test arm":                        {"fn f(n: i32) -> i32 {\n match n {\n #[cfg(test)]\n 0 => 1,\n _ => 2,\n }\n}\nfn later() {}\n", []string{"#[cfg(test)]"}},
		"cfg test last field":                 {"struct S {\n a: i32,\n #[cfg(test)]\n x: i32\n}\nfn later() {}\n", []string{"#[cfg(test)]"}},
		"generic test fn":                     {"#[test]\nfn a<A, B>() where A: Copy, B: Copy { let _ = 1; }\nfn later() {}\n", []string{"#[test]"}},
		"test_case":                           {"#[test_case(1)]\nfn a(x: i32) {}\n#[wasm_bindgen_test]\nfn b() {}\n#[rstest]\nfn c() {}\n", []string{"#[test_case(1)]", "#[wasm_bindgen_test]", "#[rstest]"}},
		"cfg test with not":                   {"#[cfg(all(test, not(miri)))]\nmod a {}\n#[cfg(all(test, not(feature = \"x\")))]\nmod b {}\n", []string{"#[cfg(all(test, not(miri)))]", "#[cfg(all(test, not(feature = \"x\")))]"}},
		"cfg strings are not test":            {"#[cfg(feature = \"test-util\")]\nmod a {}\n#[cfg(feature=\"test\")]\nmod b {}\n#[cfg(target_os = \"test\")]\nmod c {}\n", nil},
		"default fn":                          {"#[test]\ndefault fn h<A, B>() { body }\nfn later() {}\n", []string{"#[test]"}},
		"pub crate split":                     {"#[test]\npub\n(crate) fn h<A, B>() {}\nfn later() {}\n", []string{"#[test]"}},
		"signature with array":                {"#[test]\nfn a() -> [u8; 2] { [0; 2] }\nfn b() {}\n", []string{"#[test]"}},
		"attribute text in string or comment": {"// #[test]\nconst S: &str = \"#[cfg(test)]\";\nconst R: &str = r#\"#[test]\"#;\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := rustTestItems(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			var heads []string
			for _, item := range got {
				heads = append(heads, strings.SplitN(item, "\n", 2)[0])
			}
			if !slices.Equal(heads, tc.want) {
				t.Errorf("items = %q, want %q", heads, tc.want)
			}
			if name == "tricky tokens" && !strings.HasSuffix(got[0], "}\n}") && !strings.HasSuffix(got[0], "}") {
				t.Errorf("item ends wrongly: %q", got[0])
			}
			if name == "tricky tokens" && strings.Contains(got[0], "after") {
				t.Errorf("item runs past its module: %q", got[0])
			}
		})
	}
}

func TestRustTestItemsRefusesWhatItCannotMatch(t *testing.T) {
	for name, src := range map[string]string{
		"open module":      "#[cfg(test)]\nmod tests {\n    fn a() {}\n",
		"unclosed string":  "#[test]\nfn a() { let s = \"x; }\n",
		"stray close":      "#[test]\nfn a() { ) }\n",
		"unterminated raw": "#[test]\nfn a() { let s = r#\"x\"; }\n",
		"no item":          "#[test]\n",
	} {
		if _, err := rustTestItems(src); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestInlineRustTests(t *testing.T) {
	lib := "pub fn f() -> i32 { 1 }\n\n#[cfg(test)]\nmod tests {\n    #[test]\n    fn a() { assert_eq!(super::f(), 1); }\n}\n"
	cases := map[string]struct {
		base, solution map[string]string
		want           []string
	}{
		"test added": {
			map[string]string{"src/lib.rs": "pub fn f() -> i32 { 1 }\n"}, map[string]string{"src/lib.rs": lib}, []string{"src/lib.rs"}},
		"test changed": {
			map[string]string{"src/lib.rs": lib}, map[string]string{"src/lib.rs": strings.Replace(lib, "1);", "2);", 1)}, []string{"src/lib.rs"}},
		"test removed": {
			map[string]string{"src/lib.rs": lib}, map[string]string{"src/lib.rs": "pub fn f() -> i32 { 1 }\n"}, []string{"src/lib.rs"}},
		"file deleted": {
			map[string]string{"src/lib.rs": lib, "src/b.rs": "fn b() {}\n"}, map[string]string{"src/lib.rs": "", "src/b.rs": "fn b() { }\n"}, []string{"src/lib.rs"}},
		"code changes beside untouched tests": {
			map[string]string{"src/lib.rs": lib}, map[string]string{"src/lib.rs": strings.Replace(lib, "{ 1 }\n\n", "{ 2 }\n\n", 1)}, nil},
		"tests in tests dir": {
			map[string]string{"src/lib.rs": "pub fn f() {}\n", "tests/t.rs": "#[test]\nfn a() {}\n"},
			map[string]string{"src/lib.rs": "pub fn f() { }\n", "tests/t.rs": "#[test]\nfn a() { }\n"}, nil},
		"mod tests declared": {
			map[string]string{"src/lib.rs": "pub fn f() {}\n"},
			map[string]string{"src/lib.rs": "pub fn f() {}\n#[cfg(test)]\nmod tests;\n", "src/tests.rs": "#[test]\nfn a() {}\n"}, []string{"src/lib.rs", "src/tests.rs"}},
		"not rust": {
			map[string]string{"a.py": "x = 1\n"}, map[string]string{"a.py": "#[test]\nx = 2\n"}, nil},
		"unbalanced": {
			map[string]string{"src/lib.rs": "fn a() {}\n"}, map[string]string{"src/lib.rs": "#[cfg(test)]\nmod t {\n"}, []string{"src/lib.rs (unbalanced brackets: '{' opened on line 2 is never closed)"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			git(t, repo, "init", "-q", "-b", "main")
			b := commit(t, repo, tc.base, "base")
			s := commit(t, repo, tc.solution, "solution")
			ctx := context.Background()
			_, reference, err := Split(ctx, b, s, "-C", repo)
			if err != nil {
				t.Fatal(err)
			}
			got, err := InlineRustTests(ctx, b, s, reference, "-C", repo)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("files = %q, want %q", got, tc.want)
			}
			refused := RefuseInlineRustTests(ctx, b, s, reference, "-C", repo)
			if (refused != nil) != (len(tc.want) > 0) || refused != nil && !strings.Contains(refused.Error(), tc.want[0]) {
				t.Errorf("refusal = %v", refused)
			}
		})
	}
}

// A generic head with commas must not end the item early: the body belongs to it.
func TestRustTestItemsKeepGenericHeadAndBody(t *testing.T) {
	for _, src := range []string{"#[test]\ndefault fn h<A, B>() { body }", "#[test]\npub\n(crate) fn h<A, B>() { body }"} {
		got, err := rustTestItems(src + "\nfn later() {}\n")
		if err != nil || len(got) != 1 || got[0] != src {
			t.Errorf("items = %q, %v; want %q", got, err, src)
		}
	}
}
