package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseTOML parses the subset of TOML htadb's config.toml needs: tables
// ([a.b."c d"]), dotted and quoted keys, basic and literal strings,
// integers, floats, booleans, arrays (may span lines) and inline tables.
func ParseTOML(src string) (map[string]any, error) {
	root := map[string]any{}
	cur := root
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		lineNo := i + 1
		line := strings.TrimSpace(stripComment(lines[i]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[[") {
			return nil, fmt.Errorf("config.toml:%d: arrays of tables are not supported", lineNo)
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("config.toml:%d: bad table header", lineNo)
			}
			keys, err := parseKey(strings.TrimSpace(line[1 : len(line)-1]))
			if err != nil {
				return nil, fmt.Errorf("config.toml:%d: %v", lineNo, err)
			}
			t, err := descend(root, keys)
			if err != nil {
				return nil, fmt.Errorf("config.toml:%d: %v", lineNo, err)
			}
			cur = t
			continue
		}
		eq := findEquals(line)
		if eq < 0 {
			return nil, fmt.Errorf("config.toml:%d: expected key = value", lineNo)
		}
		keys, err := parseKey(strings.TrimSpace(line[:eq]))
		if err != nil {
			return nil, fmt.Errorf("config.toml:%d: %v", lineNo, err)
		}
		val := strings.TrimSpace(line[eq+1:])
		// Multi-line arrays: keep reading until brackets balance.
		for depth(val) > 0 && i+1 < len(lines) {
			i++
			val += " " + strings.TrimSpace(stripComment(lines[i]))
		}
		v, rest, err := parseValue(val)
		if err != nil {
			return nil, fmt.Errorf("config.toml:%d: %v", lineNo, err)
		}
		if strings.TrimSpace(rest) != "" {
			return nil, fmt.Errorf("config.toml:%d: trailing characters %q", lineNo, rest)
		}
		t, err := descend(cur, keys[:len(keys)-1])
		if err != nil {
			return nil, fmt.Errorf("config.toml:%d: %v", lineNo, err)
		}
		last := keys[len(keys)-1]
		if _, dup := t[last]; dup {
			return nil, fmt.Errorf("config.toml:%d: duplicate key %q", lineNo, last)
		}
		t[last] = v
	}
	return root, nil
}

func descend(t map[string]any, keys []string) (map[string]any, error) {
	for _, k := range keys {
		next, ok := t[k]
		if !ok {
			m := map[string]any{}
			t[k] = m
			t = m
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("key %q is not a table", k)
		}
		t = m
	}
	return t, nil
}

// stripComment removes a # comment outside strings.
func stripComment(s string) string {
	q := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '#':
			return s[:i]
		}
	}
	return s
}

func findEquals(s string) int {
	q := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '=':
			return i
		}
	}
	return -1
}

func depth(s string) int {
	d := 0
	q := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '[' || c == '{':
			d++
		case c == ']' || c == '}':
			d--
		}
	}
	return d
}

func parseKey(s string) ([]string, error) {
	var keys []string
	for len(s) > 0 {
		s = strings.TrimLeft(s, " \t")
		var k string
		switch {
		case strings.HasPrefix(s, `"`):
			v, rest, err := parseBasicString(s)
			if err != nil {
				return nil, err
			}
			k, s = v, rest
		case strings.HasPrefix(s, `'`):
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated key")
			}
			k, s = s[1:1+end], s[2+end:]
		default:
			end := strings.IndexAny(s, ". \t")
			if end < 0 {
				end = len(s)
			}
			k, s = s[:end], s[end:]
			for _, r := range k {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
					return nil, fmt.Errorf("bad bare key %q", k)
				}
			}
			if k == "" {
				return nil, fmt.Errorf("empty key")
			}
		}
		keys = append(keys, k)
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			break
		}
		if s[0] != '.' {
			return nil, fmt.Errorf("bad key near %q", s)
		}
		s = s[1:]
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("empty key")
	}
	return keys, nil
}

func parseBasicString(s string) (string, string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			return b.String(), s[i+1:], nil
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"', '\\':
			b.WriteByte(s[i])
		case 'u', 'U':
			n := 4
			if s[i] == 'U' {
				n = 8
			}
			if i+n >= len(s) {
				return "", "", fmt.Errorf("bad unicode escape")
			}
			r, err := strconv.ParseUint(s[i+1:i+1+n], 16, 32)
			if err != nil {
				return "", "", fmt.Errorf("bad unicode escape")
			}
			b.WriteRune(rune(r))
			i += n
		default:
			return "", "", fmt.Errorf("bad escape \\%c", s[i])
		}
	}
	return "", "", fmt.Errorf("unterminated string")
}

func parseValue(s string) (any, string, error) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return nil, "", fmt.Errorf("missing value")
	}
	switch s[0] {
	case '"':
		return parseBasicStringAny(s)
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return nil, "", fmt.Errorf("unterminated string")
		}
		return s[1 : 1+end], s[2+end:], nil
	case '[':
		var arr []any
		s = strings.TrimLeft(s[1:], " \t")
		for {
			if strings.HasPrefix(s, "]") {
				return arr, s[1:], nil
			}
			v, rest, err := parseValue(s)
			if err != nil {
				return nil, "", err
			}
			arr = append(arr, v)
			s = strings.TrimLeft(rest, " \t")
			if strings.HasPrefix(s, ",") {
				s = strings.TrimLeft(s[1:], " \t")
			} else if !strings.HasPrefix(s, "]") {
				return nil, "", fmt.Errorf("expected , or ] in array")
			}
		}
	case '{':
		m := map[string]any{}
		s = strings.TrimLeft(s[1:], " \t")
		for {
			if strings.HasPrefix(s, "}") {
				return m, s[1:], nil
			}
			eq := findEquals(s)
			if eq < 0 {
				return nil, "", fmt.Errorf("bad inline table")
			}
			keys, err := parseKey(strings.TrimSpace(s[:eq]))
			if err != nil {
				return nil, "", err
			}
			v, rest, err := parseValue(s[eq+1:])
			if err != nil {
				return nil, "", err
			}
			t, err := descend(m, keys[:len(keys)-1])
			if err != nil {
				return nil, "", err
			}
			t[keys[len(keys)-1]] = v
			s = strings.TrimLeft(rest, " \t")
			if strings.HasPrefix(s, ",") {
				s = strings.TrimLeft(s[1:], " \t")
			} else if !strings.HasPrefix(s, "}") {
				return nil, "", fmt.Errorf("expected , or } in inline table")
			}
		}
	}
	end := strings.IndexAny(s, ",]} \t")
	if end < 0 {
		end = len(s)
	}
	tok, rest := s[:end], s[end:]
	switch tok {
	case "true":
		return true, rest, nil
	case "false":
		return false, rest, nil
	}
	clean := strings.ReplaceAll(tok, "_", "")
	if n, err := strconv.ParseInt(clean, 0, 64); err == nil {
		return n, rest, nil
	}
	if f, err := strconv.ParseFloat(clean, 64); err == nil {
		return f, rest, nil
	}
	return nil, "", fmt.Errorf("bad value %q", tok)
}

func parseBasicStringAny(s string) (any, string, error) {
	v, rest, err := parseBasicString(s)
	return v, rest, err
}
