package ghjira

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Object is a JSON object from GitHub, Jira, or ADF. Numbers decode as json.Number.
type Object = map[string]any

// canonical encodes v like Python's json.dumps(sort_keys=True, ensure_ascii=False).
// Digests, operation tokens, and Jira identity labels depend on these exact bytes,
// so they stay compatible with state and labels written by earlier releases.
// With indent, it uses two spaces per level, as the state and report files do.
func canonical(v any, indent bool) []byte {
	var b bytes.Buffer
	writeValue(&b, generic(v), indent, 0)

	return b.Bytes()
}

// digest is the SHA-256 hex of v's compact canonical JSON.
func digest(v any) string {
	sum := sha256.Sum256(canonical(v, false))

	return hex.EncodeToString(sum[:])
}

// sameJSON reports whether two values have identical canonical JSON.
func sameJSON(a, b any) bool {
	return bytes.Equal(canonical(a, false), canonical(b, false))
}

// generic converts typed values (structs, typed maps) into JSON-shaped values.
func generic(v any) any {
	switch v.(type) {
	case nil, bool, string, json.Number, int, int64, float64, Object, []any:
		return v
	}
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("canonical: %v", err))
	}

	return decodeJSON(data)
}

func decodeJSON(data []byte) any {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		panic(fmt.Sprintf("canonical: %v", err))
	}

	return out
}

func writeValue(b *bytes.Buffer, v any, indent bool, depth int) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeString(b, x)
	case json.Number:
		b.WriteString(x.String())
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		b.WriteString(pythonFloat(x))
	case Object:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // UTF-8 byte order equals Python's code point order
		b.WriteByte('{')
		for i, k := range keys {
			separate(b, i, indent, depth+1)
			writeString(b, k)
			b.WriteByte(':')
			if indent {
				b.WriteByte(' ')
			}
			writeValue(b, generic(x[k]), indent, depth+1)
		}
		closeContainer(b, len(keys), indent, depth, '}')
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			separate(b, i, indent, depth+1)
			writeValue(b, generic(item), indent, depth+1)
		}
		closeContainer(b, len(x), indent, depth, ']')
	default:
		writeValue(b, generic(x), indent, depth)
	}
}

func separate(b *bytes.Buffer, i int, indent bool, depth int) {
	if i > 0 {
		b.WriteByte(',')
	}
	if indent {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat("  ", depth))
	}
}

func closeContainer(b *bytes.Buffer, n int, indent bool, depth int, end byte) {
	if indent && n > 0 {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat("  ", depth))
	}
	b.WriteByte(end)
}

// writeString escapes only what Python escapes with ensure_ascii=False.
func writeString(b *bytes.Buffer, s string) {
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
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// pythonFloat matches repr() for the floats tests construct; API numbers stay json.Number.
func pythonFloat(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e16 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}

	return strconv.FormatFloat(f, 'g', -1, 64)
}
