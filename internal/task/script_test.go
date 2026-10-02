package task

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestScriptLexerBlanksCommentsAndStrings(t *testing.T) {
	py := "x = \"in_string\" + 'also_in' # in_comment\n'''doc_name\nmore_doc'''\nreal_name = f\"{inner_name}\"\n"
	got := usedNames(py, langPython)
	for _, n := range []string{"x", "real_name"} {
		if !got[n] {
			t.Errorf("python: %s missing from %v", n, got)
		}
	}
	for _, n := range []string{"in_string", "also_in", "in_comment", "doc_name", "more_doc", "inner_name"} {
		if got[n] {
			t.Errorf("python: %s read as code", n)
		}
	}
	ts := "const a = 'q_str'; // c_line\n/* c_block */ const b = `t_${inner_expr + `nested`}_end`; const c = \"it's\";\nconst last_name = 1;\n"
	got = usedNames(ts, langTS)
	for _, n := range []string{"a", "b", "c", "last_name"} {
		if !got[n] {
			t.Errorf("ts: %s missing from %v", n, got)
		}
	}
	for _, n := range []string{"q_str", "c_line", "c_block", "t_", "inner_expr", "nested", "_end"} {
		if got[n] {
			t.Errorf("ts: %s read as code", n)
		}
	}
	// A stray quote ends at its line, so one regular expression or JSX apostrophe cannot swallow the file.
	if got := usedNames("const r = /'/;\nconst after_name = 1;\n", langTS); !got["after_name"] {
		t.Errorf("a stray quote swallowed the next line: %v", got)
	}
}

func TestFairnessPythonAndTypeScriptIdentifiers(t *testing.T) {
	const instruction = "Add the feature, exposed through RetryPolicy and RetryOptions."
	cases := map[string]struct {
		base, solution map[string]string
		want           []string
	}{
		"python: a new function, constant, field and attribute": {
			map[string]string{
				"app/core.py":        "def old_helper():\n    return 1\n",
				"tests/test_core.py": "from app.core import old_helper\n\ndef test_old():\n    assert old_helper() == 1\n",
				"app/unrelated.py":   "shared_word = 1\n",
			},
			map[string]string{
				"app/core.py": "from dataclasses import dataclass\n\nMAX_RETRIES = 3\n\ndef old_helper():\n    return 1\n\ndef brand_new_helper():\n    return 2\n\n" +
					"def shared_word_user():\n    pass\n\n@dataclass\nclass RetryPolicy:\n    jitter_ms: int = 0\n\n    def __repr__(self):\n        return 'x'\n\n    def next_delay(self):\n        return 1\n",
				"tests/test_core.py": "import os\nimport pytest\nfrom pathlib import Path\nfrom app.core import old_helper, brand_new_helper, MAX_RETRIES, RetryPolicy\n\n" +
					"def local_stub():\n    pass\n\ndef test_old():\n    assert old_helper() == 1\n\n" +
					"def test_new(tmp_path):\n    # comment_only_name\n    policy = RetryPolicy(jitter_ms=5)\n    assert policy.next_delay() == 1 and brand_new_helper() == 2 and MAX_RETRIES == 3\n    with pytest.raises(ValueError):\n        raise ValueError('doc_only_name')\n    assert Path(os.getcwd()) and local_stub() is None\n",
			},
			[]string{"MAX_RETRIES", "brand_new_helper", "jitter_ms", "next_delay"},
		},
		"python: stated, test-own, old, base, dependency and dunder names are fine": {
			map[string]string{
				"app/core.py":     "def old_helper():\n    return 1\n",
				"app/other.py":    "def base_word():\n    pass\n",
				"tests/test_a.py": "from app.core import old_helper\n\ndef test_old():\n    assert old_helper()\n",
			},
			map[string]string{
				"app/core.py":  "def old_helper():\n    return 1\n\ndef base_word():\n    pass\n\ndef local_stub():\n    pass\n\ndef RetryPolicy():\n    pass\n\nclass Other:\n    def __eq__(self, o):\n        return True\n",
				"app/other.py": "def base_word():\n    pass\n",
				"tests/test_a.py": "import json\nfrom collections import OrderedDict\nfrom app.core import old_helper, base_word, RetryPolicy, Other\n\n" +
					"def local_stub():\n    pass\n\ndef test_old(monkeypatch):\n    RetryPolicy(); base_word(); local_stub(); OrderedDict(); json.dumps({}); Other().__eq__(1)\n    assert old_helper()\n",
			},
			[]string{"Other"},
		},
		"typescript: exports, members and type names": {
			map[string]string{
				"src/core.ts":        "export function oldFn() { return 1; }\n",
				"tests/core.test.ts": "import { oldFn } from '../src/core';\nimport { it, expect } from 'vitest';\nit('old', () => { expect(oldFn()).toBe(1); });\n",
			},
			map[string]string{
				"src/core.ts": "export function oldFn() { return 1; }\nexport function brandNewFn(): number { return 2; }\nexport const MAX_RETRIES = 3;\n" +
					"export interface RetryOptions {\n  jitterMs: number;\n}\nexport class Policy {\n  nextDelay(n: number): number {\n    return n;\n  }\n  private readonly secretSalt = 1;\n}\n",
				"tests/core.test.ts": "import { oldFn, brandNewFn, MAX_RETRIES, RetryOptions, Policy } from '../src/core';\nimport { it, expect, vi } from 'vitest';\nimport { z } from 'zod';\n" +
					"const localThing = 1;\nfunction localFn(secretSalt: number) { return secretSalt; }\n" +
					"it('old', () => { expect(oldFn()).toBe(1); });\n// comment_only_name\n" +
					"it('new', () => {\n  const opts: RetryOptions = { jitterMs: 1 };\n  expect(brandNewFn() + MAX_RETRIES + localThing + localFn(1)).toBe(opts.jitterMs);\n  expect(new Policy().nextDelay(1)).toBe(vi.fn());\n  z.string();\n});\n",
			},
			[]string{"MAX_RETRIES", "Policy", "brandNewFn", "jitterMs", "nextDelay"},
		},
		"typescript and javascript share a family; type-only and named exports": {
			map[string]string{
				"src/a.js":        "export function oldFn() {}\n",
				"tests/a.spec.ts": "import { oldFn } from '../src/a';\ntest('o', () => oldFn());\n",
			},
			map[string]string{
				"src/a.js":        "export function oldFn() {}\nfunction hiddenImpl() {}\nexport { hiddenImpl as publicName };\nexport type AliasedType = string;\n",
				"tests/a.spec.ts": "import { oldFn, publicName, AliasedType } from '../src/a';\nimport type { Other } from 'dep';\ntest('o', () => { oldFn(); publicName(); const x: AliasedType = 'a'; });\n",
			},
			[]string{"AliasedType", "publicName"},
		},
		"typescript: nothing is flagged when every name is stated, local or old": {
			map[string]string{
				"src/core.ts":        "export function oldFn() { return 1; }\n",
				"tests/core.test.ts": "import { oldFn } from '../src/core';\ntest('o', () => oldFn());\n",
			},
			map[string]string{
				"src/core.ts":        "export function oldFn() { return 1; }\nexport function RetryPolicy() {}\nexport function inner(callbackArg: number) { const innerLocal = 1; return callbackArg; }\n",
				"tests/core.test.ts": "import { oldFn, RetryPolicy } from '../src/core';\ntest('o', () => { RetryPolicy(); const innerLocal = 2; const f = (callbackArg: number) => callbackArg; f(innerLocal); oldFn(); });\n",
			},
			nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var want []string
			for _, w := range tc.want {
				want = append(want, "identifier:"+w)
			}
			wantGaps(t, fairnessGaps(t, tc.base, tc.solution, instruction), want...)
		})
	}
}

func TestInlineTestsPythonAndTypeScript(t *testing.T) {
	doc := "def f():\n    \"\"\"Double.\n\n    >>> f(2)\n    4\n    \"\"\"\n    return 4\n"
	vitest := "export function f() { return 1; }\n\nif (import.meta.vitest) {\n  const { it, expect } = import.meta.vitest;\n  it('f', () => { expect(f()).toBe(1); });\n}\n"
	cases := map[string]struct {
		base, solution map[string]string
		want           []string
	}{
		"doctest added": {
			map[string]string{"src/a.py": "def f():\n    return 4\n"}, map[string]string{"src/a.py": doc}, []string{"src/a.py"}},
		"doctest changed": {
			map[string]string{"src/a.py": doc}, map[string]string{"src/a.py": strings.Replace(doc, "4\n    \"\"\"", "5\n    \"\"\"", 1)}, []string{"src/a.py"}},
		"doctest removed": {
			map[string]string{"src/a.py": doc}, map[string]string{"src/a.py": "def f():\n    return 4\n"}, []string{"src/a.py"}},
		"code changes beside untouched doctests": {
			map[string]string{"src/a.py": doc}, map[string]string{"src/a.py": strings.Replace(doc, "return 4", "return 2 * 2", 1)}, nil},
		"prose changes without examples": {
			map[string]string{"src/a.py": "def f():\n    \"\"\"Double.\"\"\"\n"}, map[string]string{"src/a.py": "def f():\n    \"\"\"Double a number.\"\"\"\n"}, nil},
		"an example in a comment is not run": {
			map[string]string{"src/a.py": "def f():\n    pass\n"}, map[string]string{"src/a.py": "def f():\n    # >>> f()\n    pass\n"}, nil},
		"a doctest in a test file is a hidden test": {
			map[string]string{"src/a.py": "def f():\n    pass\n", "tests/test_a.py": "def test_a():\n    pass\n"},
			map[string]string{"src/a.py": "def f():\n    return 1\n", "tests/test_a.py": strings.Replace(doc, "def f", "def test_a", 1)}, nil},
		"in-source test added": {
			map[string]string{"src/a.ts": "export function f() { return 1; }\n"}, map[string]string{"src/a.ts": vitest}, []string{"src/a.ts"}},
		"in-source test changed": {
			map[string]string{"src/a.ts": vitest}, map[string]string{"src/a.ts": strings.Replace(vitest, "toBe(1)", "toBe(2)", 1)}, []string{"src/a.ts"}},
		"code changes beside untouched in-source tests": {
			map[string]string{"src/a.ts": vitest}, map[string]string{"src/a.ts": strings.Replace(vitest, "return 1; }\n\nif", "return 1 + 0; }\n\nif", 1)}, nil},
		"the guard in a string or comment is refused with the file (the lexer cannot tell it from a hidden one)": {
			map[string]string{"src/a.ts": "export const a = 1;\n"},
			map[string]string{"src/a.ts": "export const a = 2; // if (import.meta.vitest) { x }\n"}, []string{"src/a.ts (an in-source test guard (import.meta.vitest) was seen 1 times but 0 could be parsed)"}},
		"a backtick in a regex before the guard": {
			map[string]string{"src/a.ts": "export const a = 1;\n"},
			map[string]string{"src/a.ts": "export const a = (s) => s.replace(/`/g, '');\n" + vitest}, []string{"src/a.ts (an in-source test guard (import.meta.vitest) was seen 2 times but 0 could be parsed)"}},
		"a slash-star in a regex before the guard": {
			map[string]string{"src/a.ts": "export const a = 1;\n"},
			map[string]string{"src/a.ts": "export const a = /a\\/*.ts/;\n" + vitest}, []string{"src/a.ts (an in-source test guard (import.meta.vitest) was seen 2 times but 0 could be parsed)"}},
		"a character class with slash-star before the guard": {
			map[string]string{"src/a.ts": "export const a = 1;\n"},
			map[string]string{"src/a.ts": "export const a = /[/*]/;\n" + vitest}, []string{"src/a.ts (an in-source test guard (import.meta.vitest) was seen 2 times but 0 could be parsed)"}},
		"unbalanced in-source block": {
			map[string]string{"src/a.ts": "export const a = 1;\n"},
			map[string]string{"src/a.ts": "if (import.meta.vitest) {\n  it('x', () => {});\n"}, []string{"src/a.ts (unbalanced braces: \"{\" opened on line 1 is never closed)"}},
		"plain test file under tests": {
			map[string]string{"src/a.ts": "export const a = 1;\n", "tests/a.test.ts": "it('a', () => {});\n"},
			map[string]string{"src/a.ts": "export const a = 2;\n", "tests/a.test.ts": "it('a', () => { expect(1); });\n"}, nil},
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
			_, py, ts, err := InlineTests(ctx, b, s, reference, "-C", repo)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Concat(py, ts); !slices.Equal(got, tc.want) {
				t.Errorf("files = %q, want %q", got, tc.want)
			}
			refused := RefuseInlineRustTests(ctx, b, s, reference, "-C", repo)
			if (refused != nil) != (len(tc.want) > 0) || refused != nil && !strings.Contains(refused.Error(), tc.want[0]) {
				t.Errorf("refusal = %v", refused)
			}
		})
	}
}

// The refusal names the kind and says what to do, for each language.
func TestRefuseInlineTestsMessages(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	b := commit(t, repo, map[string]string{"a.py": "x = 1\n", "b.ts": "export const b = 1;\n"}, "base")
	s := commit(t, repo, map[string]string{"a.py": "def f():\n    \"\"\"\n    >>> f()\n    \"\"\"\n", "b.ts": "if (import.meta.vitest) {}\n"}, "solution")
	_, reference, err := Split(context.Background(), b, s, "-C", repo)
	if err != nil {
		t.Fatal(err)
	}
	err = RefuseInlineRustTests(context.Background(), b, s, reference, "-C", repo)
	if err == nil {
		t.Fatal("not refused")
	}
	for _, want := range []string{"changes Python doctests inside source files (a.py)", "changes Vitest in-source tests (import.meta.vitest) inside source files (b.ts)", "move them to a file under tests/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
