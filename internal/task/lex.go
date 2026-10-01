package task

import (
	"strings"
	"unicode/utf8"
)

// lang selects the lexical rules lexAt applies.
type lang int

const (
	langJava lang = iota
	langKotlin
	langRust
)

// token kinds returned by lexAt.
const (
	tokNone    = iota // not a comment, string or character literal
	tokComment        // // or /* */ (nested in Kotlin and Rust)
	tokString         // "..." with backslash escapes
	tokRaw            // no escapes: Java and Kotlin """...""", Rust r"..." and r#"..."#
	tokChar           // 'x' or '\n' (a Rust lifetime is not one)
)

// lexAt recognizes a comment, string or character literal starting at src[i]. It returns the offset just past it and,
// for strings, the bounds of the text between the delimiters. Unterminated input runs to the end (a Java or Kotlin
// "..." stops at its line's end instead, so one stray quote cannot swallow the file). It is a deliberately small lexer:
// it knows enough to skip what could hide a brace or a literal, nothing more.
func lexAt(src string, i int, l lang) (end, bodyStart, bodyEnd, kind int) {
	switch c := src[i]; {
	case c == '/' && i+1 < len(src) && src[i+1] == '/':
		j := strings.IndexByte(src[i:], '\n')
		if j < 0 {
			return len(src), 0, 0, tokComment
		}
		return i + j, 0, 0, tokComment
	case c == '/' && i+1 < len(src) && src[i+1] == '*':
		depth, j := 1, i+2
		for j < len(src) && depth > 0 {
			switch {
			case strings.HasPrefix(src[j:], "*/"):
				depth--
				j += 2
			case l != langJava && strings.HasPrefix(src[j:], "/*"):
				depth++
				j += 2
			default:
				j++
			}
		}
		return j, 0, 0, tokComment
	case c == '"' && l != langRust && strings.HasPrefix(src[i:], `"""`):
		if j := strings.Index(src[i+3:], `"""`); j >= 0 {
			// Kotlin allows extra quotes before the closing delimiter ("""a"""" is a"); they belong to the text.
			e := i + 3 + j
			for l == langKotlin && e+3 < len(src) && src[e+3] == '"' {
				e++
			}
			return e + 3, i + 3, e, tokRaw
		}
		return len(src), i + 3, len(src), tokRaw
	case c == '"':
		j := i + 1
		for j < len(src) && src[j] != '"' {
			if src[j] == '\\' {
				j++
			} else if src[j] == '\n' && l != langRust {
				return j, i + 1, j, tokString
			}
			j++
		}
		if j >= len(src) {
			return len(src), i + 1, len(src), tokString
		}
		return j + 1, i + 1, j, tokString
	case c == '\'':
		if i+1 < len(src) && src[i+1] == '\\' {
			j := min(i+3, len(src)) // past the escaped character, so '\'' ends at its last quote
			for j < len(src) && src[j] != '\'' && src[j] != '\n' {
				j++
			}
			if j < len(src) && src[j] == '\'' {
				return j + 1, 0, 0, tokChar
			}
			return i + 1, 0, 0, tokNone
		}
		if _, n := utf8.DecodeRuneInString(src[min(i+1, len(src)):]); n > 0 && i+1+n < len(src) && src[i+1+n] == '\'' && src[i+1] != '\'' {
			return i + 2 + n, 0, 0, tokChar
		}
	case l == langRust && (c == 'r' || c == 'b' || c == 'c') && (i == 0 || !isIdentByte(src[i-1])):
		j := i
		if c != 'r' {
			j++
		}
		if j < len(src) && src[j] == 'r' {
			j++
			hashes := 0
			for j < len(src) && src[j] == '#' {
				hashes++
				j++
			}
			if j < len(src) && src[j] == '"' {
				closing := `"` + strings.Repeat("#", hashes)
				if k := strings.Index(src[j+1:], closing); k >= 0 {
					return j + 1 + k + len(closing), j + 1, j + 1 + k, tokRaw
				}
				return len(src), j + 1, len(src), tokRaw
			}
		}
	}
	return i, 0, 0, tokNone
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// stringLiterals returns the text of every string literal of a Java, Kotlin or Rust file, comments and character
// literals skipped. Escapes are decoded in the common cases (\n, \t, \", \', \\, a Rust line continuation); other
// escapes keep the escaped character as a letter (\u{1F600} becomes u{1F600}, \x41 becomes x41, \0 becomes 0, Java
// \s becomes s). A Kotlin $name or ${...} template inside a string becomes a line break, so it separates pieces the way
// a format verb does. Limits: the lexer ends a string at the first inner quote, so "${map["key"]} tail" is cut
// there and code fragments such as key"]} tail come back as literals (a stated or base text hides them; a
// reference that produces one would flag it); a Java text block keeps its indentation (the fairness check
// normalizes whitespace anyway).
func stringLiterals(src string, l lang) []string {
	var out []string
	for i := 0; i < len(src); {
		end, bs, be, kind := lexAt(src, i, l)
		switch kind {
		case tokString:
			out = append(out, decodeEscapes(src[bs:be], l, true))
		case tokRaw:
			if l == langRust {
				out = append(out, src[bs:be])
			} else {
				out = append(out, decodeEscapes(src[bs:be], l, false))
			}
		}
		if end <= i {
			end = i + 1
		}
		i = end
	}
	return out
}

// decodeEscapes decodes backslash escapes when escapes is set, and for Kotlin replaces templates with line breaks. A
// Java text block decodes escapes too (escapes true); a Kotlin raw string does not, but still has templates.
func decodeEscapes(s string, l lang, escapes bool) string {
	if l == langJava && !escapes {
		escapes = true // a Java text block processes escapes
	}
	if !strings.ContainsAny(s, `\$`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && escapes && i+1 < len(s):
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\n':
				if l == langRust { // line continuation: the break and the next line's indentation are dropped
					for i+1 < len(s) && strings.IndexByte(" \t\r\n", s[i+1]) >= 0 {
						i++
					}
				}
			default:
				b.WriteByte(s[i])
			}
		case c == '$' && l == langKotlin && i+1 < len(s) && s[i+1] == '{':
			depth, j := 0, i+1
			for ; j < len(s); j++ {
				if s[j] == '{' {
					depth++
				} else if s[j] == '}' {
					if depth--; depth == 0 {
						break
					}
				}
			}
			b.WriteByte('\n')
			i = j
		case c == '$' && l == langKotlin && i+1 < len(s) && isIdentByte(s[i+1]):
			j := i + 1
			for j+1 < len(s) && isIdentByte(s[j+1]) {
				j++
			}
			b.WriteByte('\n')
			i = j
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
