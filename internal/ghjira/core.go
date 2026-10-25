// Package ghjira creates Jira issues from GitHub issues and copies comments both ways.
//
// Configuration, stable operation identities, and conservative text conversion live here.
package ghjira

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// SyncError is an expected, reportable failure; its message is shown verbatim.
type SyncError struct{ Msg string }

func (e *SyncError) Error() string { return e.Msg }

func syncErr(format string, args ...any) error {
	return &SyncError{Msg: fmt.Sprintf(format, args...)}
}

// HTTPStatusError is a non-2xx answer. Response bodies can contain issue text or
// credentials, so they are never included.
type HTTPStatusError struct {
	Status int
	Msg    string
}

func (e *HTTPStatusError) Error() string { return e.Msg }

// refusedStatus returns the status when the server answered with a 4xx (other than
// a 408 timeout), proving the write never took effect.
func refusedStatus(err error) (int, bool) {
	var h *HTTPStatusError
	if errors.As(err, &h) && h.Status >= 400 && h.Status < 500 && h.Status != 408 {
		return h.Status, true
	}
	return 0, false
}

// Config is a validated sync.json.
type Config struct {
	Repository       string
	GitHubURL        string
	JiraURL          string
	JiraProject      string
	JiraIssueType    string
	SelectionLabel   *string // nil explicitly opts into all issues
	IssueState       string
	MaxOperations    int
	MaxPages         int
	FullRescanHours  int
	Comments         bool
	CommentsSince    *time.Time
	JiraCreateFields Object
}

var (
	repositoryPattern = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
	projectPattern    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	markerPattern     = regexp.MustCompile(`^\[gh-jira-sync:([a-f0-9]{64})\]$`)
)

var configKeys = map[string]bool{
	"repository": true, "github_url": true, "jira_url": true, "jira_project": true,
	"jira_issue_type": true, "selection_label": true, "issue_state": true, "max_operations": true,
	"max_pages": true, "comments": true, "comments_since": true, "jira_create_fields": true,
	"full_rescan_hours": true,
}

// ParseConfig validates sync.json. Unknown keys and wrong types fail closed.
func ParseConfig(data []byte) (*Config, error) {
	var top any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, err
	}
	raw, ok := top.(Object)
	if !ok {
		return nil, syncErr("Unknown configuration keys or invalid configuration")
	}
	for k := range raw {
		if !configKeys[k] {
			return nil, syncErr("Unknown configuration keys or invalid configuration")
		}
	}
	value := func(key string, fallback any) any {
		if v, present := raw[key]; present {
			return v
		}
		return fallback
	}
	c := &Config{}
	var str bool
	if c.Repository, str = raw["repository"].(string); !str || !repositoryPattern.MatchString(c.Repository) {
		return nil, syncErr("repository must be owner/name")
	}
	for _, key := range []string{"github_url", "jira_url"} {
		fallback := any(nil)
		if key == "github_url" {
			fallback = "https://api.github.com"
		}
		s, isString := value(key, fallback).(string)
		if !isString {
			return nil, syncErr("%s is required", key)
		}
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, syncErr("%s must be an HTTPS URL without credentials, query, or fragment", key)
		}
		if key == "github_url" {
			c.GitHubURL = strings.TrimRight(s, "/")
		} else {
			c.JiraURL = strings.TrimRight(s, "/")
		}
	}
	if c.JiraProject, str = raw["jira_project"].(string); !str || !projectPattern.MatchString(c.JiraProject) {
		return nil, syncErr("jira_project must be a project key")
	}
	if c.JiraIssueType, str = raw["jira_issue_type"].(string); !str || !isDigits(c.JiraIssueType) {
		return nil, syncErr("jira_issue_type must be an issue type ID string")
	}
	switch label := value("selection_label", "sync-jira").(type) {
	case nil:
	case string:
		if strings.TrimFunc(label, isPythonSpace) == "" || strings.Contains(label, ",") {
			return nil, syncErr("selection_label must be one label or null (all issues)")
		}
		c.SelectionLabel = &label
	default:
		return nil, syncErr("selection_label must be one label or null (all issues)")
	}
	c.IssueState, _ = value("issue_state", "open").(string)
	if c.IssueState != "open" && c.IssueState != "closed" && c.IssueState != "all" {
		return nil, syncErr("issue_state must be open, closed, or all")
	}
	for _, field := range []struct {
		key      string
		fallback int
		target   *int
	}{{"max_operations", 20, &c.MaxOperations}, {"max_pages", 100, &c.MaxPages}, {"full_rescan_hours", 24, &c.FullRescanHours}} {
		n, ok := strictInt(value(field.key, json.Number(strconv.Itoa(field.fallback))))
		if !ok || n < 1 {
			return nil, syncErr("%s must be a positive integer", field.key)
		}
		*field.target = n
	}
	if c.Comments, ok = value("comments", true).(bool); !ok {
		return nil, syncErr("comments must be boolean")
	}
	if since := value("comments_since", nil); since != nil {
		s, isString := since.(string)
		t, err := parseTimestamp(s)
		if !isString || err != nil {
			return nil, syncErr("comments_since must be an ISO timestamp with timezone")
		}
		c.CommentsSince = &t
	}
	extra, ok := value("jira_create_fields", Object{}).(Object)
	if !ok {
		return nil, syncErr("jira_create_fields cannot override identity, summary, description, or labels")
	}
	for _, k := range []string{"summary", "description", "project", "issuetype", "labels"} {
		if _, present := extra[k]; present {
			return nil, syncErr("jira_create_fields cannot override identity, summary, description, or labels")
		}
	}
	c.JiraCreateFields = extra
	return c, nil
}

// strictInt accepts only JSON integers, as Python's `type(x) is int` does.
func strictInt(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(n.String())
	return i, err == nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isPythonSpace matches str.isspace, which also counts \x1c-\x1f.
func isPythonSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// isPythonLineBreak matches the separators str.splitlines recognizes.
func isPythonLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

var timestampLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05.999999999Z07", "2006-01-02T15:04Z07:00", "2006-01-02T15:04Z0700",
}

// parseTimestamp accepts ISO 8601 timestamps that carry a timezone, including Jira's
// "+0000" offsets. Naive timestamps are rejected.
func parseTimestamp(s string) (time.Time, error) {
	candidates := []string{s}
	if len(s) > 10 && s[10] == ' ' {
		candidates = append(candidates, s[:10]+"T"+s[11:])
	}
	for _, candidate := range candidates {
		for _, layout := range timestampLayouts {
			if t, err := time.Parse(layout, candidate); err == nil {
				return t, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp")
}

// binding identifies the deployment; changing any of these fields needs new state.
func binding(c *Config) string {
	return digest(Object{"repository": c.Repository, "github_url": c.GitHubURL,
		"jira_url": c.JiraURL, "jira_project": c.JiraProject})
}

// token is the stable operation identity for one source object.
func token(c *Config, kind string, sourceID any) string {
	return digest([]any{binding(c), kind, idString(sourceID)})
}

func marker(op string) string { return "[gh-jira-sync:" + op + "]" }

// toADF copies text literally: one ADF paragraph per line.
func toADF(text string) Object {
	lines := strings.Split(text, "\n")
	content := make([]any, len(lines))
	for i, line := range lines {
		children := []any{}
		if line != "" {
			children = []any{Object{"type": "text", "text": line}}
		}
		content[i] = Object{"type": "paragraph", "content": children}
	}
	return Object{"type": "doc", "version": 1, "content": content}
}

var plainMarks = map[string]bool{"strong": true, "em": true, "strike": true, "underline": true,
	"code": true, "textColor": true, "backgroundColor": true, "subsup": true}

var blockSeparators = map[string]string{"doc": "\n", "paragraph": "", "heading": "", "blockquote": "\n",
	"codeBlock": "", "bulletList": "\n", "orderedList": "\n", "listItem": "\n"}

// adfText renders readable plain text and never silently discards unsupported nodes or marks.
func adfText(doc any) (string, error) {
	d, ok := doc.(Object)
	if !ok || d["type"] != "doc" || !isOne(d["version"]) {
		return "", syncErr("Unsupported Jira comment document")
	}
	return renderADF(d)
}

func renderADF(node Object) (string, error) {
	t, _ := node["type"].(string)
	switch t {
	case "text":
		text, ok := node["text"].(string)
		if !ok {
			return "", syncErr("Unsupported Jira comment content; copy manually")
		}
		for _, m := range list(node["marks"]) {
			kind, _ := asObject(m)["type"].(string)
			if kind == "link" {
				href, ok := asObject(asObject(m)["attrs"])["href"].(string)
				if !ok {
					return "", syncErr("Unsupported Jira comment mark")
				}
				text += " (" + href + ")"
			} else if !plainMarks[kind] {
				return "", syncErr("Unsupported Jira comment mark")
			}
		}
		return text, nil
	case "hardBreak":
		return "\n", nil
	case "rule":
		return "---", nil
	case "mention":
		if text, ok := asObject(node["attrs"])["text"].(string); ok {
			return text, nil
		}
		return "[mention]", nil
	case "emoji":
		attrs := asObject(node["attrs"])
		if text, _ := attrs["text"].(string); text != "" {
			return text, nil
		}
		if short, ok := attrs["shortName"].(string); ok {
			return short, nil
		}
		return "[emoji]", nil
	}
	separator, block := blockSeparators[t]
	if !block {
		return "", syncErr("Unsupported Jira comment content; copy manually")
	}
	var parts []string
	for _, child := range list(node["content"]) {
		c, ok := child.(Object)
		if !ok {
			return "", syncErr("Unsupported Jira comment content; copy manually")
		}
		part, err := renderADF(c)
		if err != nil {
			return "", err
		}
		parts = append(parts, part)
	}
	text := strings.Join(parts, separator)
	if t == "listItem" {
		return "- " + text, nil
	}
	return text, nil
}

// normalized unifies GitHub web-editor CRLF line endings so hashes stay stable and ADF text clean.
func normalized(body any) string {
	s, _ := body.(string)
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func commentText(side string, comment Object) (string, error) {
	if side == "github" {
		return normalized(comment["body"]), nil
	}
	return adfText(comment["body"])
}

// trailingMarker returns the operation in a marker that is the body's own last line,
// where copies carry it. Quote-replies embed a copy's marker inside a quote ("> [...]"
// or an ADF blockquote), so they remain originals. Only the last top-level ADF
// paragraph is read, which works even when the rest of the comment uses unsupported ADF.
func trailingMarker(side string, comment Object) string {
	var last string
	if side == "github" {
		body, _ := comment["body"].(string)
		body = strings.TrimRightFunc(body, isPythonSpace)
		if i := strings.LastIndexFunc(body, isPythonLineBreak); i >= 0 {
			body = body[i:]
			_, size := firstRune(body)
			body = body[size:]
		}
		last = strings.TrimFunc(body, isPythonSpace)
	} else {
		var texts []*string // nil entries are non-paragraph nodes
		for _, node := range list(asObject(comment["body"])["content"]) {
			n, ok := node.(Object)
			if !ok || n["type"] != "paragraph" {
				texts = append(texts, nil)
				continue
			}
			var b strings.Builder
			for _, leaf := range list(n["content"]) {
				if l, ok := leaf.(Object); ok {
					s, _ := l["text"].(string)
					b.WriteString(s)
				}
			}
			s := b.String()
			texts = append(texts, &s)
		}
		for len(texts) > 0 && texts[len(texts)-1] != nil && strings.TrimFunc(*texts[len(texts)-1], isPythonSpace) == "" {
			texts = texts[:len(texts)-1]
		}
		if len(texts) > 0 && texts[len(texts)-1] != nil {
			last = strings.TrimFunc(*texts[len(texts)-1], isPythonSpace)
		}
	}
	if m := markerPattern.FindStringSubmatch(last); m != nil {
		return m[1]
	}
	return ""
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}

// hasMarker reports a trailing marker at all (op == "") or for one operation.
func hasMarker(side string, comment Object, op string) bool {
	found := trailingMarker(side, comment)
	if op == "" {
		return found != ""
	}
	return found == op
}

func restricted(comment Object) bool {
	if truthy(comment["visibility"]) || comment["jsdPublic"] == false {
		return true
	}
	for _, p := range list(comment["properties"]) {
		prop := asObject(p)
		if prop["key"] == "sd.public.comment" && asObject(prop["value"])["internal"] == true {
			return true
		}
	}
	return false
}

func createdAt(comment Object) string {
	if s, _ := comment["created_at"].(string); s != "" {
		return s
	}
	s, _ := comment["created"].(string)
	return s
}

func eligibleComment(c *Config, comment Object) (bool, error) {
	if c.CommentsSince == nil {
		return true, nil
	}
	created, err := parseTimestamp(createdAt(comment))
	if err != nil {
		return false, syncErr("Comment creation time is missing or invalid")
	}
	return !created.Before(*c.CommentsSince), nil
}

// JSON helpers for loosely typed API objects; wrong shapes read as empty values.

func asObject(v any) Object {
	o, _ := v.(Object)
	return o
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case Object:
		return len(x) > 0
	case []any:
		return len(x) > 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	}
	return true
}

func isOne(v any) bool {
	switch x := v.(type) {
	case int:
		return x == 1
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == 1
	case float64:
		return x == 1
	}
	return false
}

// idString renders IDs the way Python's str() does for ints and strings.
func idString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return pythonFloat(x)
	case nil:
		return "None"
	}
	return fmt.Sprint(v)
}

func runeLen(s string) int { return len([]rune(s)) }
