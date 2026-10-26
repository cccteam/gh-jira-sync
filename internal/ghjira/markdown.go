package ghjira

import (
	"regexp"
	"strconv"
	"strings"
)

// markdownADF renders GitHub-flavored markdown as an Atlassian Document Format
// document, for issue descriptions. It covers what issue bodies commonly use:
// headings, paragraphs (single newlines are line breaks, as GitHub shows them),
// bullet, ordered and task lists with nesting, block quotes, fenced code, rules,
// tables, and inline strong, emphasis, strikethrough, code, links and bare URLs.
// Anything else stays as literal text, so nothing is dropped. HTML comments, which
// GitHub hides, are removed.
func markdownADF(md string) Object {
	md = htmlComment.ReplaceAllString(normalized(md), "")
	content := mdBlocks(strings.Split(md, "\n"))
	if len(content) == 0 {
		content = []any{Object{"type": "paragraph", "content": []any{}}}
	}
	return Object{"type": "doc", "version": 1, "content": content}
}

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	fenceLine   = regexp.MustCompile("^ {0,3}(```+|~~~+)\\s*([^`\\s]*)")
	headingLine = regexp.MustCompile(`^ {0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	ruleLine    = regexp.MustCompile(`^ {0,3}(-(\s*-){2,}|\*(\s*\*){2,}|_(\s*_){2,})\s*$`)
	quoteLine   = regexp.MustCompile(`^ {0,3}> ?(.*)$`)
	itemLine    = regexp.MustCompile(`^( *)([-*+]|\d{1,9}[.)])( +|$)(.*)$`)
	tableRule   = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$`)
	taskBox     = regexp.MustCompile(`^\[([ xX])\]\s+`)
)

func mdBlocks(lines []string) []any {
	var blocks []any
	for i := 0; i < len(lines); {
		line := lines[i]
		switch {
		case strings.TrimSpace(line) == "":
			i++
		case fenceLine.MatchString(line):
			m := fenceLine.FindStringSubmatch(line)
			var code []string
			j := i + 1
			for ; j < len(lines); j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), m[1][:3]) {
					break
				}
				code = append(code, lines[j])
			}
			block := Object{"type": "codeBlock", "content": []any{}}
			if text := strings.Join(code, "\n"); text != "" {
				block["content"] = []any{Object{"type": "text", "text": text}}
			}
			if m[2] != "" {
				block["attrs"] = Object{"language": m[2]}
			}
			blocks = append(blocks, block)
			i = j + 1
		case headingLine.MatchString(line):
			m := headingLine.FindStringSubmatch(line)
			blocks = append(blocks, Object{"type": "heading", "attrs": Object{"level": len(m[1])},
				"content": mdInline(m[2])})
			i++
		case ruleLine.MatchString(line):
			blocks = append(blocks, Object{"type": "rule"})
			i++
		case quoteLine.MatchString(line):
			var quoted []string
			for ; i < len(lines) && quoteLine.MatchString(lines[i]); i++ {
				quoted = append(quoted, quoteLine.FindStringSubmatch(lines[i])[1])
			}
			if inner := mdBlocks(quoted); len(inner) > 0 {
				blocks = append(blocks, Object{"type": "blockquote", "content": inner})
			}
		case itemLine.MatchString(line):
			var list Object
			list, i = mdList(lines, i)
			blocks = append(blocks, list)
		case i+1 < len(lines) && strings.Contains(line, "|") && tableRule.MatchString(lines[i+1]):
			var table Object
			table, i = mdTable(lines, i)
			blocks = append(blocks, table)
		default:
			var para []string
			for ; i < len(lines) && strings.TrimSpace(lines[i]) != ""; i++ {
				if len(para) > 0 && startsBlock(lines[i]) {
					break
				}
				para = append(para, strings.TrimSpace(lines[i]))
			}
			blocks = append(blocks, mdParagraph(para))
		}
	}
	return blocks
}

func startsBlock(line string) bool {
	return fenceLine.MatchString(line) || headingLine.MatchString(line) || ruleLine.MatchString(line) ||
		quoteLine.MatchString(line) || itemLine.MatchString(line)
}

// mdParagraph joins lines with hard breaks, as GitHub renders newlines in issues.
func mdParagraph(lines []string) Object {
	content := []any{}
	for i, line := range lines {
		if i > 0 {
			content = append(content, Object{"type": "hardBreak"})
		}
		content = append(content, mdInline(line)...)
	}
	return Object{"type": "paragraph", "content": content}
}

func ordered(marker string) bool { return marker[len(marker)-1] == '.' || marker[len(marker)-1] == ')' }

// mdList reads one list starting at lines[i]: items at its indent with the same kind
// of marker, each holding the more-indented lines after it as nested blocks.
func mdList(lines []string, i int) (Object, int) {
	first := itemLine.FindStringSubmatch(lines[i])
	indent, isOrdered := len(first[1]), ordered(first[2])
	list := Object{"type": "bulletList"}
	if isOrdered {
		list["type"] = "orderedList"
		if n, err := strconv.Atoi(strings.TrimRight(first[2], ".)")); err == nil && n != 1 {
			list["attrs"] = Object{"order": n}
		}
	}
	var items []any
	var body []string
	contentIndent := 0
	flush := func() {
		if body == nil {
			return
		}
		blocks := mdBlocks(body)
		if len(blocks) == 0 {
			blocks = []any{Object{"type": "paragraph", "content": []any{}}}
		}
		items = append(items, Object{"type": "listItem", "content": blocks})
	}
	for ; i < len(lines); i++ {
		line := lines[i]
		lead := len(line) - len(strings.TrimLeft(line, " "))
		if strings.TrimSpace(line) == "" {
			// A blank line ends the list unless the list or the item continues after it.
			next := i + 1
			for next < len(lines) && strings.TrimSpace(lines[next]) == "" {
				next++
			}
			if next == len(lines) {
				break
			}
			nextLead := len(lines[next]) - len(strings.TrimLeft(lines[next], " "))
			m := itemLine.FindStringSubmatch(lines[next])
			if nextLead < contentIndent && (m == nil || len(m[1]) != indent || ordered(m[2]) != isOrdered) {
				break
			}
			body = append(body, "")
			continue
		}
		if m := itemLine.FindStringSubmatch(line); m != nil && lead <= indent+1 && lead < contentIndent || m != nil && body == nil {
			if len(m[1]) != indent && body != nil || ordered(m[2]) != isOrdered {
				break
			}
			flush()
			text := m[4]
			if box := taskBox.FindStringSubmatch(text); box != nil {
				mark := "☐ "
				if box[1] != " " {
					mark = "☑ "
				}
				text = mark + text[len(box[0]):]
			}
			body = []string{text}
			contentIndent = indent + len(m[2]) + len(m[3])
			if m[3] == "" {
				contentIndent++
			}
			continue
		}
		if lead >= contentIndent {
			body = append(body, line[min(lead, contentIndent):])
			continue
		}
		// A lazy continuation line extends the item's paragraph; anything else ends the list.
		if body[len(body)-1] != "" && !startsBlock(line) {
			body = append(body, strings.TrimSpace(line))
			continue
		}
		break
	}
	flush()
	list["content"] = items
	return list, i
}

func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(strings.TrimSuffix(line, "|"), "|")
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cell.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

func mdTable(lines []string, i int) (Object, int) {
	header := tableCells(lines[i])
	row := func(cells []string, kind string) Object {
		var content []any
		for c := range header {
			text := ""
			if c < len(cells) {
				text = cells[c]
			}
			content = append(content, Object{"type": kind, "content": []any{mdParagraph([]string{text})}})
		}
		return Object{"type": "tableRow", "content": content}
	}
	rows := []any{row(header, "tableHeader")}
	for i += 2; i < len(lines) && strings.TrimSpace(lines[i]) != "" && strings.Contains(lines[i], "|"); i++ {
		rows = append(rows, row(tableCells(lines[i]), "tableCell"))
	}
	return Object{"type": "table", "content": rows}, i
}

var (
	inlineLink = regexp.MustCompile(`^!?\[((?:[^\[\]]|\[[^\]]*\])*)\]\(\s*<?([^\s)>]+)>?(?:\s+"[^"]*")?\s*\)`)
	bareURL    = regexp.MustCompile(`^https?://[^\s<>]*[^\s<>.,;:!?'")\]]`)
	angleURL   = regexp.MustCompile(`^<(https?://[^\s>]+)>`)
	lineBreak  = regexp.MustCompile(`(?i)^<br\s*/?>`)
)

type mark = Object

// mdInline parses inline markdown into ADF text nodes.
func mdInline(s string) []any {
	return mergeText(inline(s, nil))
}

func withMark(marks []mark, m mark) []mark {
	return append(append([]mark{}, marks...), m)
}

func inline(s string, marks []mark) []any {
	var out []any
	var text strings.Builder
	emit := func() {
		if text.Len() > 0 {
			node := Object{"type": "text", "text": text.String()}
			if len(marks) > 0 {
				node["marks"] = toAny(marks)
			}
			out = append(out, node)
			text.Reset()
		}
	}
	for i := 0; i < len(s); {
		rest := s[i:]
		switch {
		case rest[0] == '\\' && len(rest) > 1 && strings.ContainsRune("\\`*_{}[]()#+-.!|~<>", rune(rest[1])):
			text.WriteByte(rest[1])
			i += 2
			continue
		case rest[0] == '`':
			ticks := len(rest) - len(strings.TrimLeft(rest, "`"))
			if end := strings.Index(rest[ticks:], rest[:ticks]); end >= 0 {
				emit()
				code := strings.TrimSpace(rest[ticks : ticks+end])
				// ADF allows only a link alongside the code mark.
				codeMarks := []mark{{"type": "code"}}
				for _, m := range marks {
					if m["type"] == "link" {
						codeMarks = append(codeMarks, m)
					}
				}
				if code != "" {
					out = append(out, Object{"type": "text", "text": code, "marks": toAny(codeMarks)})
				}
				i += 2*ticks + end
				continue
			}
		case rest[0] == '[' || strings.HasPrefix(rest, "!["):
			if m := inlineLink.FindStringSubmatch(rest); m != nil {
				emit()
				label := m[1]
				if label == "" {
					label = m[2]
				}
				out = append(out, inline(label, withMark(marks, mark{"type": "link", "attrs": Object{"href": m[2]}}))...)
				i += len(m[0])
				continue
			}
		case rest[0] == '<':
			if m := angleURL.FindStringSubmatch(rest); m != nil {
				emit()
				out = append(out, inline(m[1], withMark(marks, mark{"type": "link", "attrs": Object{"href": m[1]}}))...)
				i += len(m[0])
				continue
			}
			if m := lineBreak.FindString(rest); m != "" {
				emit()
				out = append(out, Object{"type": "hardBreak"})
				i += len(m)
				continue
			}
		case rest[0] == 'h' && !linked(marks) && (i == 0 || !isWordByte(s[i-1])):
			if m := bareURL.FindString(rest); m != "" {
				emit()
				out = append(out, Object{"type": "text", "text": m,
					"marks": toAny(withMark(marks, mark{"type": "link", "attrs": Object{"href": m}}))})
				i += len(m)
				continue
			}
		case rest[0] == '*' || rest[0] == '_' || rest[0] == '~':
			if n, kind, inner, ok := emphasis(s, i); ok {
				emit()
				out = append(out, inline(inner, withMark(marks, mark{"type": kind}))...)
				i += n
				continue
			}
		}
		text.WriteByte(rest[0])
		i++
	}
	emit()
	return out
}

func linked(marks []mark) bool {
	for _, m := range marks {
		if m["type"] == "link" {
			return true
		}
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// emphasis matches a delimited run at s[i]: ** or __ (strong), * or _ (em), ~~
// (strike). Underscores only count at word boundaries, so snake_case stays text.
func emphasis(s string, i int) (int, string, string, bool) {
	for _, d := range []struct{ delim, kind string }{
		{"**", "strong"}, {"__", "strong"}, {"~~", "strike"}, {"*", "em"}, {"_", "em"},
	} {
		if !strings.HasPrefix(s[i:], d.delim) {
			continue
		}
		start := i + len(d.delim)
		if start >= len(s) || s[start] == ' ' {
			continue
		}
		underscore := d.delim[0] == '_'
		if underscore && i > 0 && isWordByte(s[i-1]) {
			continue
		}
		for j := start + 1; j+len(d.delim) <= len(s); j++ {
			if !strings.HasPrefix(s[j:], d.delim) || s[j-1] == ' ' || s[j-1] == '\\' {
				continue
			}
			// A single * or _ must not be half of a double delimiter.
			if len(d.delim) == 1 && j+1 < len(s) && s[j+1] == d.delim[0] {
				j++
				continue
			}
			end := j + len(d.delim)
			if underscore && end < len(s) && isWordByte(s[end]) {
				continue
			}
			return end - i, d.kind, s[start:j], true
		}
	}
	return 0, "", "", false
}

func toAny(marks []mark) []any {
	out := make([]any, len(marks))
	for i, m := range marks {
		out[i] = m
	}
	return out
}

// mergeText joins adjacent text nodes with identical marks.
func mergeText(nodes []any) []any {
	var out []any
	for _, n := range nodes {
		node := n.(Object)
		if len(out) > 0 && node["type"] == "text" {
			prev := out[len(out)-1].(Object)
			if prev["type"] == "text" && string(canonical(prev["marks"], false)) == string(canonical(node["marks"], false)) {
				prev["text"] = prev["text"].(string) + node["text"].(string)
				continue
			}
		}
		out = append(out, node)
	}
	if out == nil {
		out = []any{}
	}
	return out
}
