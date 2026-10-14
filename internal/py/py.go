// Package py is Python 3's text model where looper's ports need it to match byte for byte:
// how text mode decodes bytes, what str.splitlines() and str.strip() do, and json.dumps(v,
// indent=2) with ensure_ascii. Go's strings and encoding/json disagree with all of these.
package py

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pinepeakdigital/looper/internal/record"
)

// Decode is how Python's text mode reads bytes: UTF-8, then universal newlines (\r\n and a
// lone \r both become \n). Python's strict decoder dies on invalid UTF-8; this reads the bad
// bytes as U+FFFD instead (a divergence, see README).
func Decode(b []byte) string {
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// isLineBreak is the set str.splitlines() splits on.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// Splitlines is str.splitlines(): no trailing empty line, \r\n is one break.
func Splitlines(s string) []string {
	var out []string
	start := 0
	for i, r := range s {
		if i < start || !isLineBreak(r) {
			continue
		}
		out = append(out, s[start:i])
		start = i + utf8.RuneLen(r)
		if r == '\r' && start < len(s) && s[start] == '\n' {
			start++
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Strip is str.strip(): Python's whitespace, from both ends.
func Strip(s string) string { return strings.TrimFunc(s, record.PySpace) }

// Value is what the JSON encoder takes: string, int, bool, nil, []value, *string (nil is
// null) or obj, whose fields keep the order they were written in, as a Python dict does.
type Value any

// Field is an alias of an unnamed struct, not a named type, so the ports can write
// obj{{"key", v}} the way a dict literal reads: go vet's composites check flags unkeyed
// literals only of named struct types from another package.
type Field = struct {
	Key string
	Val Value
}

type Obj []Field

// Encode is json.dumps(v, indent=2) plus print's newline: ensure_ascii, ", " never appears
// because indent puts each item on its own line, and an empty list or object is [] or {}.
func Encode(v Value) string {
	var b strings.Builder
	write(&b, v, 0)
	b.WriteByte('\n')
	return b.String()
}

func write(b *strings.Builder, v Value, depth int) {
	pad := func(d int) { b.WriteString("\n" + strings.Repeat("  ", d)) }
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case string:
		Quote(b, x)
	case *string:
		if x == nil {
			b.WriteString("null")
		} else {
			Quote(b, *x)
		}
	case []Value:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			pad(depth + 1)
			write(b, e, depth+1)
		}
		pad(depth)
		b.WriteByte(']')
	case Obj:
		if len(x) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, f := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			pad(depth + 1)
			Quote(b, f.Key)
			b.WriteString(": ")
			write(b, f.Val, depth+1)
		}
		pad(depth)
		b.WriteByte('}')
	default:
		panic(fmt.Sprintf("py: cannot encode a %T", v))
	}
}

// Quote is json's ensure_ascii string form: the five short escapes, and \uXXXX (lowercase,
// a surrogate pair above the BMP) for every code point outside ' '..'~' — DEL included.
func Quote(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	u := func(c rune) {
		b.WriteString(`\u`)
		for sh := 12; sh >= 0; sh -= 4 {
			b.WriteByte(hex[(c>>sh)&0xf])
		}
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r >= ' ' && r <= '~':
				b.WriteRune(r)
			case r > 0xffff:
				r -= 0x10000
				u(0xd800 | (r>>10)&0x3ff)
				u(0xdc00 | r&0x3ff)
			default:
				u(r)
			}
		}
	}
	b.WriteByte('"')
}
