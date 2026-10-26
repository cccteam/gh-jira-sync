package ghjira

import (
	"strings"
	"testing"
)

func txt(s string, marks ...Object) Object {
	node := Object{"type": "text", "text": s}
	if len(marks) > 0 {
		m := make([]any, len(marks))
		for i, mk := range marks {
			m[i] = mk
		}
		node["marks"] = m
	}
	return node
}

func para(content ...any) Object { return Object{"type": "paragraph", "content": content} }

func item(blocks ...any) Object { return Object{"type": "listItem", "content": blocks} }

var (
	strong = Object{"type": "strong"}
	em     = Object{"type": "em"}
	code   = Object{"type": "code"}
)

func link(href string) Object { return Object{"type": "link", "attrs": Object{"href": href}} }

func doc(blocks ...any) Object { return Object{"type": "doc", "version": 1, "content": blocks} }

func sameADF(t *testing.T, got, want Object) {
	t.Helper()
	if g, w := string(canonical(got, false)), string(canonical(want, false)); g != w {
		t.Fatalf("got  %s\nwant %s", g, w)
	}
}

// validADF checks the ADF rules Jira enforces that a converter could break.
func validADF(t *testing.T, node Object, parent string) {
	t.Helper()
	kind, _ := node["type"].(string)
	children := list(node["content"])
	switch kind {
	case "text":
		if node["text"] == "" {
			t.Fatalf("empty text node under %s", parent)
		}
		hasCode := false
		for _, m := range list(node["marks"]) {
			hasCode = hasCode || asObject(m)["type"] == "code"
		}
		for _, m := range list(node["marks"]) {
			if mt := asObject(m)["type"]; hasCode && mt != "code" && mt != "link" {
				t.Fatalf("code mark combined with %v", mt)
			}
		}
	case "bulletList", "orderedList":
		for _, c := range children {
			if asObject(c)["type"] != "listItem" {
				t.Fatalf("%s holds %v", kind, asObject(c)["type"])
			}
		}
	case "listItem", "blockquote", "tableHeader", "tableCell":
		if len(children) == 0 {
			t.Fatalf("empty %s", kind)
		}
	case "paragraph", "heading":
		for _, c := range children {
			if ct := asObject(c)["type"]; ct != "text" && ct != "hardBreak" {
				t.Fatalf("%s holds block %v", kind, ct)
			}
		}
	}
	for _, c := range children {
		validADF(t, asObject(c), kind)
	}
}

func TestMarkdownIssueBody(t *testing.T) {
	t.Parallel()
	// The shape of the CDP plan issues.
	got := markdownADF("Instead of `CODEOWNERS`, a ruleset.\n\n- **Owner:** Evan\n- **Jira:** ITINFRA-11821\n\n" +
		"From the [CDP project plan](https://docs.google.com/x), row 5.")
	sameADF(t, got, doc(
		para(txt("Instead of "), txt("CODEOWNERS", code), txt(", a ruleset.")),
		Object{"type": "bulletList", "content": []any{
			item(para(txt("Owner:", strong), txt(" Evan"))),
			item(para(txt("Jira:", strong), txt(" ITINFRA-11821"))),
		}},
		para(txt("From the "), txt("CDP project plan", link("https://docs.google.com/x")), txt(", row 5."))))
}

func TestMarkdownBlocks(t *testing.T) {
	t.Parallel()
	got := markdownADF("## Plan\nline one\nline two\n\n> quoted **text**\n\n```go\nx := 1\n```\n\n---\n\n" +
		"3. third\n4. fourth\n   - nested\n\n- [ ] open\n- [x] done\n\n| A | B |\n|---|:-:|\n| 1 | `2` |")
	sameADF(t, got, doc(
		Object{"type": "heading", "attrs": Object{"level": 2}, "content": []any{txt("Plan")}},
		para(txt("line one"), Object{"type": "hardBreak"}, txt("line two")),
		Object{"type": "blockquote", "content": []any{para(txt("quoted "), txt("text", strong))}},
		Object{"type": "codeBlock", "attrs": Object{"language": "go"}, "content": []any{txt("x := 1")}},
		Object{"type": "rule"},
		Object{"type": "orderedList", "attrs": Object{"order": 3}, "content": []any{
			item(para(txt("third"))),
			item(para(txt("fourth")), Object{"type": "bulletList", "content": []any{item(para(txt("nested")))}}),
		}},
		Object{"type": "bulletList", "content": []any{item(para(txt("☐ open"))), item(para(txt("☑ done")))}},
		Object{"type": "table", "content": []any{
			Object{"type": "tableRow", "content": []any{
				Object{"type": "tableHeader", "content": []any{para(txt("A"))}},
				Object{"type": "tableHeader", "content": []any{para(txt("B"))}},
			}},
			Object{"type": "tableRow", "content": []any{
				Object{"type": "tableCell", "content": []any{para(txt("1"))}},
				Object{"type": "tableCell", "content": []any{para(txt("2", code))}},
			}},
		}}))
}

func TestMarkdownInline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want []any
	}{
		{"snake_case_name stays", []any{txt("snake_case_name stays")}},
		{"_em_ and *em* and ~~gone~~", []any{
			txt("em", em), txt(" and "), txt("em", em), txt(" and "),
			txt("gone", Object{"type": "strike"}),
		}},
		{"**bold with [a link](https://x.io)**", []any{txt("bold with ", strong), txt("a link", strong, link("https://x.io"))}},
		{"see https://x.io/a_b, then <https://y.io>", []any{
			txt("see "), txt("https://x.io/a_b", link("https://x.io/a_b")),
			txt(", then "), txt("https://y.io", link("https://y.io")),
		}},
		{`\*not em\* and 2 * 3 * 4`, []any{txt("*not em* and 2 * 3 * 4")}},
		{"**`code` in bold**", []any{txt("code", code), txt(" in bold", strong)}},
		{"a<br>b", []any{txt("a"), Object{"type": "hardBreak"}, txt("b")}},
		{"![diagram](https://x.io/d.png)", []any{txt("diagram", link("https://x.io/d.png"))}},
		{"unclosed **bold", []any{txt("unclosed **bold")}},
	} {
		sameADF(t, markdownADF(tc.in), doc(para(tc.want...)))
	}
}

func TestMarkdownDropsHiddenCommentsAndKeepsEverythingElse(t *testing.T) {
	t.Parallel()
	got := markdownADF("visible <!-- gh-jira-sync:abc --> text\n<!--\nmulti\n-->\n<div>raw html</div>")
	sameADF(t, got, doc(para(txt("visible  text")), para(txt("<div>raw html</div>"))))
	sameADF(t, markdownADF(""), doc(para()))
	sameADF(t, markdownADF("```\n```"), doc(Object{"type": "codeBlock", "content": []any{}}))
}

func TestMarkdownOutputIsValidADF(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"- \n- [ ] \n-", "1.\n2. x", "> \n>", "**", "``", "[]()", "| a |\n|---|\n", "#", "###### h\n####### not",
		"- a\n\n  continued\n- b\nlazy\n\n  - c\n    1. d\n\ntext", "* * *", "~~~\ncode\n~~~", strings.Repeat("- x\n  ", 40),
		"*a **b** c*", "__a _b_ c__", "[**x**](https://x.io)", "`a``b`", "a\\", "<", "http://", "https://x.io.",
	}
	for _, in := range inputs {
		d := markdownADF(in)
		validADF(t, d, "")
	}
}
