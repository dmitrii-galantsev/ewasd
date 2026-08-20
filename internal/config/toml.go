package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// tomlValue is one key's parsed right-hand side: either a quoted string or
// a single-line array of quoted strings. line is the 1-indexed source line
// the key appeared on, retained so type-mismatch errors (e.g. "workspace"
// given as an array) can still name a location.
type tomlValue struct {
	line    int
	str     string
	array   []string
	isArray bool
}

// keyValueRe matches "key = <rest>" lines. Only bare identifier-ish keys
// are supported (letters, digits, underscore, hyphen) — enough for
// "workspace" and "remote_keys" and any similarly-shaped key a future
// version might add. Anything that doesn't match this shape at all,
// including a bare "[section]" table header, falls through to the
// "unsupported syntax" error in parseTOML.
var keyValueRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)[ \t]*=[ \t]*(.*)$`)

// parseTOML parses content (the text of the file at path, used only for
// error messages) as ewasd's strict TOML subset: comments, blank lines,
// `key = "string"`, and `key = ["a", "b"]` (single-line arrays, trailing
// comma allowed). Anything else — table headers, bare ints/bools/dates,
// multiline arrays, inline tables — is rejected with an error naming path
// and the 1-indexed line number. There is no partial success: the first
// bad line aborts the whole parse.
func parseTOML(path, content string) (map[string]tomlValue, error) {
	values := map[string]tomlValue{}
	lines := strings.Split(content, "\n")
	for i, rawLine := range lines {
		lineNo := i + 1
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		match := keyValueRe.FindStringSubmatch(trimmed)
		if match == nil {
			return nil, tomlErr(path, lineNo, "unsupported syntax (only blank lines, '# comments', 'key = \"value\"', and 'key = [\"a\", \"b\"]' are supported): %s", trimmed)
		}
		key, rest := match[1], strings.TrimSpace(match[2])
		if rest == "" {
			return nil, tomlErr(path, lineNo, "missing value for key %q", key)
		}
		switch rest[0] {
		case '"':
			str, tail, ok := parseQuotedString(rest)
			if !ok {
				return nil, tomlErr(path, lineNo, "unterminated or invalid quoted string for key %q", key)
			}
			if err := requireOnlyTrailingComment(tail); err != nil {
				return nil, tomlErr(path, lineNo, "unexpected content after value for key %q: %s", key, tail)
			}
			values[key] = tomlValue{line: lineNo, str: str}
		case '[':
			array, tail, err := parseInlineArray(rest)
			if err != nil {
				return nil, tomlErr(path, lineNo, "key %q: %s", key, err.Error())
			}
			if err := requireOnlyTrailingComment(tail); err != nil {
				return nil, tomlErr(path, lineNo, "unexpected content after array for key %q: %s", key, tail)
			}
			values[key] = tomlValue{line: lineNo, array: array, isArray: true}
		default:
			return nil, tomlErr(path, lineNo, "unsupported value for key %q (only quoted strings and single-line arrays of quoted strings are supported; ints, bools, dates, tables, and multiline arrays are not): %s", key, rest)
		}
	}
	return values, nil
}

// parseQuotedString parses a double-quoted string starting at s[0] == '"',
// handling \" \\ \n \t escapes. It returns the decoded value and the
// remainder of s after the closing quote. ok is false for an unterminated
// string or an unrecognized escape sequence — this parser rejects rather
// than guesses at anything it isn't sure about.
func parseQuotedString(s string) (value, tail string, ok bool) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '\\' {
			if i+1 >= len(s) {
				return "", "", false
			}
			switch s[i+1] {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				return "", "", false
			}
			i += 2
			continue
		}
		if c == '"' {
			return b.String(), s[i+1:], true
		}
		b.WriteByte(c)
		i++
	}
	return "", "", false
}

// parseInlineArray parses a single-line array of quoted strings starting
// at s[0] == '['. A trailing comma before the closing ']' is accepted. It
// returns the elements and the remainder of s after the closing ']'. An
// array that isn't closed before the line ends is an error: this parser
// deliberately does not support multiline arrays.
func parseInlineArray(s string) (elements []string, tail string, err error) {
	rest := s[1:]
	elements = []string{}
	for {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			return nil, "", errors.New("unterminated array (arrays must be closed on the same line)")
		}
		if rest[0] == ']' {
			return elements, rest[1:], nil
		}
		if rest[0] != '"' {
			return nil, "", fmt.Errorf("array elements must be double-quoted strings, found: %s", rest)
		}
		value, next, ok := parseQuotedString(rest)
		if !ok {
			return nil, "", errors.New("unterminated or invalid quoted string inside array")
		}
		elements = append(elements, value)
		rest = strings.TrimLeft(next, " \t")
		if rest == "" {
			return nil, "", errors.New("unterminated array (arrays must be closed on the same line)")
		}
		switch rest[0] {
		case ',':
			rest = rest[1:]
			continue
		case ']':
			return elements, rest[1:], nil
		default:
			return nil, "", fmt.Errorf("expected ',' or ']' in array, found: %s", rest)
		}
	}
}

// requireOnlyTrailingComment reports an error unless s is empty/whitespace
// or a "# comment" once trimmed — i.e. there is nothing meaningful left
// after a value on its line.
func requireOnlyTrailingComment(s string) error {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil
	}
	return fmt.Errorf("trailing content: %s", trimmed)
}

func tomlErr(path string, line int, format string, args ...any) error {
	return fmt.Errorf("%s:%d: %s", path, line, fmt.Sprintf(format, args...))
}
