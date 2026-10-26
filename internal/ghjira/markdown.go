package ghjira

import (
	"bytes"
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
			blocks = append(blocks, Object{
				"type": "heading", "attrs": Object{"level": len(m[1])},
				"content": mdInline(m[2]),
			})
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

// sameList reports whether an itemLine match is an item of the list at indent.
func sameList(m []string, indent int, isOrdered bool) bool {
	return len(m) == 5 && len(m[1]) == indent && ordered(m[2]) == isOrdered
}

// mdList reads one list starting at lines[i]: items at its indent with the same kind
// of marker, each holding the more-indented lines after it as nested blocks.
func mdList(lines []string, i int) (block Object, next int) {
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
			if !continuesAfterBlank(lines, i, contentIndent, indent, isOrdered) {
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
			body = []string{taskText(m[4])}
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

// continuesAfterBlank reports whether the list goes on after the blank line at i: the
// next text is indented into the item or is another item of this list.
func continuesAfterBlank(lines []string, i, contentIndent, indent int, isOrdered bool) bool {
	next := i + 1
	for next < len(lines) && strings.TrimSpace(lines[next]) == "" {
		next++
	}
	if next == len(lines) {
		return false
	}
	nextLead := len(lines[next]) - len(strings.TrimLeft(lines[next], " "))

	return nextLead >= contentIndent || sameList(itemLine.FindStringSubmatch(lines[next]), indent, isOrdered)
}

// taskText renders a task list box ("[ ] " or "[x] ") as a ballot box character.
func taskText(text string) string {
	box := taskBox.FindStringSubmatch(text)
	if box == nil {
		return text
	}
	mark := "☐ "
	if box[1] != " " {
		mark = "☑ "
	}

	return mark + text[len(box[0]):]
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

func mdTable(lines []string, i int) (table Object, next int) {
	header := tableCells(lines[i])
	row := func(cells []string, kind string) Object {
		content := make([]any, 0, len(header))
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
		if rest[0] == '\\' && len(rest) > 1 && strings.ContainsRune("\\`*_{}[]()#+-.!|~<>", rune(rest[1])) {
			text.WriteByte(rest[1])
			i += 2

			continue
		}
		if nodes, n := span(s, i, marks); n > 0 {
			emit()
			out = append(out, nodes...)
			i += n

			continue
		}
		text.WriteByte(rest[0])
		i++
	}
	emit()

	return out
}

// span reads a structured span at s[i] (code, link, line break, bare URL, or emphasis),
// returning its nodes and length, or a length of 0 when none starts there.
func span(s string, i int, marks []mark) (nodes []any, n int) {
	rest := s[i:]
	linkTo := func(href string) []mark { return withMark(marks, mark{"type": "link", "attrs": Object{"href": href}}) }
	switch {
	case rest[0] == '`':
		return codeSpan(rest, marks)
	case rest[0] == '[' || strings.HasPrefix(rest, "!["):
		if m := inlineLink.FindStringSubmatch(rest); m != nil {
			label := m[1]
			if label == "" {
				label = m[2]
			}

			return inline(label, linkTo(m[2])), len(m[0])
		}
	case rest[0] == '<':
		if m := angleURL.FindStringSubmatch(rest); m != nil {
			return inline(m[1], linkTo(m[1])), len(m[0])
		}
		if m := lineBreak.FindString(rest); m != "" {
			return []any{Object{"type": "hardBreak"}}, len(m)
		}
	case rest[0] == 'h' && !linked(marks) && (i == 0 || !isWordByte(s[i-1])):
		if m := bareURL.FindString(rest); m != "" {
			return []any{Object{"type": "text", "text": m, "marks": toAny(linkTo(m))}}, len(m)
		}
	case rest[0] == '*' || rest[0] == '_' || rest[0] == '~':
		if n, kind, inner, ok := emphasis(s, i); ok {
			return inline(inner, withMark(marks, mark{"type": kind})), n
		}
	}

	return nil, 0
}

// codeSpan reads a backtick code span at the start of rest, returning its node (none
// for an empty span) and its length, or a length of 0 when the span is unclosed.
func codeSpan(rest string, marks []mark) (nodes []any, n int) {
	ticks := len(rest) - len(strings.TrimLeft(rest, "`"))
	end := strings.Index(rest[ticks:], rest[:ticks])
	if end < 0 {
		return nil, 0
	}
	code := strings.TrimSpace(rest[ticks : ticks+end])
	// ADF allows only a link alongside the code mark.
	codeMarks := []mark{{"type": "code"}}
	for _, m := range marks {
		if m["type"] == "link" {
			codeMarks = append(codeMarks, m)
		}
	}
	if code != "" {
		nodes = []any{Object{"type": "text", "text": code, "marks": toAny(codeMarks)}}
	}

	return nodes, 2*ticks + end
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
func emphasis(s string, i int) (n int, kind, inner string, ok bool) {
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
	out := make([]any, 0, len(nodes))
	for _, n := range nodes {
		node := asObject(n)
		if len(out) > 0 && node["type"] == "text" {
			prev := asObject(out[len(out)-1])
			if prev["type"] == "text" && bytes.Equal(canonical(prev["marks"], false), canonical(node["marks"], false)) {
				before, _ := prev["text"].(string)
				after, _ := node["text"].(string)
				prev["text"] = before + after

				continue
			}
		}
		out = append(out, node)
	}

	return out
}
