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
	"slices"
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
	Repository    string
	GitHubURL     string
	JiraURL       string
	JiraProject   string
	JiraIssueType string
	// JiraIssueTypes maps GitHub issue type names (matched ignoring case) to Jira
	// issue type IDs for new issues; others use JiraIssueType.
	JiraIssueTypes   map[string]string
	SelectionLabel   *string // nil explicitly opts into all issues
	IssueState       string
	MaxOperations    int
	MaxPages         int
	FullRescanHours  int
	Comments         bool
	CommentsSince    *time.Time
	JiraCreateFields Object
	StatusSync       *StatusSync // nil leaves issue status alone
	// SyncParents mirrors GitHub sub-issue parents and Jira parent links between
	// linked issues, such as an Epic and its stories.
	SyncParents bool
	// JiraSubtaskType is the Jira sub-task issue type ID for new issues whose GitHub
	// parent is a standard Jira issue (such as a Task under a Task); "" turns this off.
	JiraSubtaskType string
	// JiraSelectionLabel selects Jira issues in the project to create in GitHub; nil
	// disables Jira → GitHub creation.
	JiraSelectionLabel *string
	ProjectStatus      *ProjectStatus // nil disables project status mode
}

// ProjectStatus mirrors a GitHub Project's single-select field (normally Status)
// and Jira workflow statuses. It replaces StatusSync: the project drives Jira, and
// Jira's Done category still closes or reopens the GitHub issue.
type ProjectStatus struct {
	Owner         string
	OwnerType     string // "org" or "user"
	ProjectNumber int
	Field         string
	AddToProject  bool
	Map           []ProjectStatusMapping
	// JiraBoard is the board whose backlog the jira_backlog option mirrors (team-managed
	// boards keep backlog membership apart from status).
	JiraBoard int
}

// ProjectStatusMapping pairs one project option with the Jira statuses it covers.
// Jira[0] is the status Jira moves to; Transition picks one when several lead there.
type ProjectStatusMapping struct {
	Project    string
	Jira       []string
	Transition string
	// JiraBacklog marks the option that means "in the Jira board's backlog"; every
	// other option then also means "on the board".
	JiraBacklog bool
}

func (p *ProjectStatus) mapping(option string) (ProjectStatusMapping, bool) {
	for _, m := range p.Map {
		if m.Project == option {
			return m, true
		}
	}
	return ProjectStatusMapping{}, false
}

// backlogOption returns the option marked jira_backlog, or "".
func (p *ProjectStatus) backlogOption() string {
	for _, m := range p.Map {
		if m.JiraBacklog {
			return m.Project
		}
	}
	return ""
}

// optionFor returns the project option for a Jira issue: the backlog option while it
// is in the board's backlog, otherwise the option covering its status.
func (p *ProjectStatus) optionFor(status string, inBacklog bool) (string, bool) {
	if b := p.backlogOption(); b != "" && inBacklog {
		return b, true
	}
	return p.forJira(status)
}

// forJira returns the project option that covers a Jira status.
func (p *ProjectStatus) forJira(status string) (string, bool) {
	for _, m := range p.Map {
		if slices.Contains(m.Jira, status) {
			return m.Project, true
		}
	}
	return "", false
}

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

func parseProjectStatus(raw any) (*ProjectStatus, error) {
	bad := func(detail string) (*ProjectStatus, error) {
		return nil, syncErr("project_status: %s", detail)
	}
	obj, ok := raw.(Object)
	if !ok {
		return bad("must be null or an object")
	}
	allowed := map[string]bool{"owner": true, "owner_type": true, "project_number": true, "field": true, "add_to_project": true, "map": true, "jira_board": true}
	for k := range obj {
		if !allowed[k] {
			return bad("unknown key " + strconv.Quote(k))
		}
	}
	p := &ProjectStatus{OwnerType: "org", Field: "Status", AddToProject: true}
	if p.Owner, _ = obj["owner"].(string); !ownerPattern.MatchString(p.Owner) {
		return bad("owner must be a GitHub organization or user name")
	}
	if v, present := obj["owner_type"]; present {
		if p.OwnerType, _ = v.(string); p.OwnerType != "org" && p.OwnerType != "user" {
			return bad("owner_type must be org or user")
		}
	}
	if p.ProjectNumber, ok = strictInt(obj["project_number"]); !ok || p.ProjectNumber < 1 {
		return bad("project_number must be a positive integer")
	}
	if v, present := obj["field"]; present {
		if p.Field, _ = v.(string); strings.TrimSpace(p.Field) == "" {
			return bad("field must be a field name")
		}
	}
	if v, present := obj["add_to_project"]; present {
		if p.AddToProject, ok = v.(bool); !ok {
			return bad("add_to_project must be boolean")
		}
	}
	entries, _ := obj["map"].([]any)
	if len(entries) == 0 {
		return bad("map must list at least one option")
	}
	seenOption, seenJira := map[string]bool{}, map[string]bool{}
	backlogSeen := false
	for _, entry := range entries {
		e, ok := entry.(Object)
		if !ok {
			return bad("each map entry must be an object")
		}
		for k := range e {
			if k != "project" && k != "jira" && k != "transition" && k != "jira_backlog" {
				return bad("unknown map key " + strconv.Quote(k))
			}
		}
		m := ProjectStatusMapping{}
		if m.Project, _ = e["project"].(string); strings.TrimSpace(m.Project) == "" || seenOption[m.Project] {
			return bad("each map entry needs a distinct project option name")
		}
		seenOption[m.Project] = true
		// "jira" is one status name or a list; the first listed is the one Jira moves to.
		switch v := e["jira"].(type) {
		case string:
			m.Jira = []string{v}
		case []any:
			for _, s := range v {
				name, _ := s.(string)
				m.Jira = append(m.Jira, name)
			}
		}
		if v, present := e["jira_backlog"]; present {
			if m.JiraBacklog, ok = v.(bool); !ok {
				return bad("jira_backlog must be boolean")
			}
		}
		switch {
		case m.JiraBacklog && (len(m.Jira) > 0 || m.Transition != "" || e["transition"] != nil):
			return bad("the jira_backlog entry " + strconv.Quote(m.Project) + " takes no Jira statuses or transition")
		case m.JiraBacklog && backlogSeen:
			return bad("only one map entry can be jira_backlog")
		case !m.JiraBacklog && len(m.Jira) == 0:
			return bad("map entry " + strconv.Quote(m.Project) + " needs at least one Jira status")
		}
		backlogSeen = backlogSeen || m.JiraBacklog
		for _, name := range m.Jira {
			if strings.TrimSpace(name) == "" || seenJira[name] {
				return bad("Jira statuses must be non-empty and appear in only one map entry")
			}
			seenJira[name] = true
		}
		if v, present := e["transition"]; present {
			if m.Transition, _ = v.(string); strings.TrimSpace(m.Transition) == "" {
				return bad("transition must be a transition name")
			}
		}
		p.Map = append(p.Map, m)
	}
	if v, present := obj["jira_board"]; present {
		if p.JiraBoard, ok = strictInt(v); !ok || p.JiraBoard < 1 {
			return bad("jira_board must be a board ID")
		}
	}
	if backlogSeen != (p.JiraBoard != 0) {
		return bad("a jira_backlog map entry and jira_board go together")
	}
	return p, nil
}

// StatusSync names the Jira workflow transitions that mirror closing and reopening
// a GitHub issue. Jira statuses in the Done category close the GitHub issue.
type StatusSync struct {
	CloseTransition  string
	ReopenTransition string
}

var (
	repositoryPattern = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
	projectPattern    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	// markerPattern is the visible marker older releases appended to copies;
	// hiddenMarkerPattern is the HTML comment GitHub copies now end with.
	markerPattern       = regexp.MustCompile(`^\[gh-jira-sync:([a-f0-9]{64})\]$`)
	hiddenMarkerPattern = regexp.MustCompile(`^<!--\s*gh-jira-sync:([a-f0-9]{64})\s*-->$`)
)

var configKeys = map[string]bool{
	"repository": true, "github_url": true, "jira_url": true, "jira_project": true,
	"jira_issue_type": true, "selection_label": true, "issue_state": true, "max_operations": true,
	"max_pages": true, "comments": true, "comments_since": true, "jira_create_fields": true,
	"full_rescan_hours": true, "status_sync": true, "project_status": true, "jira_issue_types": true,
	"sync_parents": true, "jira_selection_label": true, "jira_subtask_type": true,
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
	c.JiraIssueTypes = map[string]string{}
	if raw, present := raw["jira_issue_types"]; present && raw != nil {
		types, ok := raw.(Object)
		if !ok {
			return nil, syncErr("jira_issue_types must map GitHub issue type names to Jira issue type ID strings")
		}
		for name, id := range types {
			idString, _ := id.(string)
			if strings.TrimSpace(name) == "" || !isDigits(idString) {
				return nil, syncErr("jira_issue_types must map GitHub issue type names to Jira issue type ID strings")
			}
			for existing := range c.JiraIssueTypes {
				if strings.EqualFold(existing, name) {
					return nil, syncErr("jira_issue_types lists %q twice (names ignore case)", name)
				}
			}
			c.JiraIssueTypes[name] = idString
		}
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
	if c.SyncParents, ok = value("sync_parents", false).(bool); !ok {
		return nil, syncErr("sync_parents must be boolean")
	}
	switch id := value("jira_subtask_type", nil).(type) {
	case nil:
	case string:
		if !isDigits(id) {
			return nil, syncErr("jira_subtask_type must be a sub-task issue type ID string or null")
		}
		// A sub-task's parent is fixed at creation and then kept by parent sync.
		if !c.SyncParents {
			return nil, syncErr("jira_subtask_type needs sync_parents")
		}
		c.JiraSubtaskType = id
	default:
		return nil, syncErr("jira_subtask_type must be a sub-task issue type ID string or null")
	}
	switch label := value("jira_selection_label", nil).(type) {
	case nil:
	case string:
		// Jira labels cannot contain spaces; quotes would break the JQL that selects them.
		if label == "" || strings.ContainsAny(label, " \"\\") {
			return nil, syncErr("jira_selection_label must be one Jira label (no spaces or quotes) or null")
		}
		c.JiraSelectionLabel = &label
	default:
		return nil, syncErr("jira_selection_label must be one Jira label (no spaces or quotes) or null")
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
		return nil, syncErr("jira_create_fields cannot override project, issue type, summary, or description")
	}
	for _, k := range []string{"summary", "description", "project", "issuetype"} {
		if _, present := extra[k]; present {
			return nil, syncErr("jira_create_fields cannot override project, issue type, summary, or description")
		}
	}
	c.JiraCreateFields = extra
	if raw, present := raw["status_sync"]; present && raw != nil {
		statusSync, ok := raw.(Object)
		closeName, _ := statusSync["close_transition"].(string)
		reopenName, _ := statusSync["reopen_transition"].(string)
		if !ok || len(statusSync) != 2 || strings.TrimSpace(closeName) == "" || strings.TrimSpace(reopenName) == "" {
			return nil, syncErr("status_sync must be null or {\"close_transition\": name, \"reopen_transition\": name}")
		}
		c.StatusSync = &StatusSync{CloseTransition: closeName, ReopenTransition: reopenName}
	}
	if raw, present := raw["project_status"]; present && raw != nil {
		if c.StatusSync != nil {
			// Both would move Jira when a GitHub issue closes, possibly to different statuses.
			return nil, syncErr("Use either status_sync or project_status, not both")
		}
		ps, err := parseProjectStatus(raw)
		if err != nil {
			return nil, err
		}
		c.ProjectStatus = ps
	}
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
func token(c *Config, kind OpKind, sourceID string) string {
	return digest([]any{binding(c), string(kind), sourceID})
}

// marker is the visible marker older releases appended to copies. It is still
// recognized, and golden parity tests pin its format.
func marker(op string) string { return "[gh-jira-sync:" + op + "]" }

// hiddenMarker ends GitHub copies: an HTML comment, which GitHub does not render.
func hiddenMarker(op string) string { return "<!-- gh-jira-sync:" + op + " -->" }

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

// issueDescription is the Jira description: a link back, then the GitHub body as written.
func issueDescription(source *GitHubIssue) string {
	return "From GitHub: " + source.HTMLURL + "\n\n" + normalized(source.Body)
}

// normalized unifies GitHub web-editor CRLF line endings so hashes stay stable and ADF text clean.
func normalized(body string) string {
	return strings.ReplaceAll(body, "\r\n", "\n")
}

// trailingMarker returns the copy operation a comment carries, or "".
//
// Jira copies carry it in a comment property (Identity), invisible to readers.
// GitHub copies end with a hidden HTML comment marker on their own last line.
// Copies from older releases end with a visible marker line, still recognized.
// Quote-replies embed a copy's marker inside a quote ("> ..." or an ADF blockquote)
// and never carry the property, so they remain originals. Only the last top-level
// ADF paragraph is read, which works even when the rest uses unsupported ADF.
func trailingMarker(comment Comment) string {
	if comment.Identity != "" {
		return comment.Identity
	}
	var last string
	if comment.Side == SideGitHub {
		body, _ := comment.Body.(string)
		body = strings.TrimRightFunc(body, isPythonSpace)
		if i := strings.LastIndexFunc(body, isPythonLineBreak); i >= 0 {
			body = body[i:]
			_, size := firstRune(body)
			body = body[size:]
		}
		last = strings.TrimFunc(body, isPythonSpace)
	} else {
		var texts []*string // nil entries are non-paragraph nodes
		for _, node := range list(asObject(comment.Body)["content"]) {
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
	if m := hiddenMarkerPattern.FindStringSubmatch(last); m != nil && comment.Side == SideGitHub {
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
func hasMarker(comment Comment, op string) bool {
	found := trailingMarker(comment)
	if op == "" {
		return found != ""
	}
	return found == op
}

func eligibleComment(c *Config, comment Comment) (bool, error) {
	if c.CommentsSince == nil {
		return true, nil
	}
	created, err := parseTimestamp(comment.Created)
	if err != nil {
		return false, syncErr("Comment creation time is missing or invalid")
	}
	return !created.Before(*c.CommentsSince), nil
}

// Helpers for walking raw ADF documents, the one deliberately untyped structure:
// wrong shapes read as empty values and are then rejected as unsupported content.

func asObject(v any) Object {
	o, _ := v.(Object)
	return o
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
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

func runeLen(s string) int { return len([]rune(s)) }

// issueTypeFor returns the Jira issue type ID for a new issue: the one mapped to its
// GitHub issue type, or jira_issue_type when it has no type or an unmapped one.
func (c *Config) issueTypeFor(source *GitHubIssue) string {
	if source.Type != nil {
		for name, id := range c.JiraIssueTypes {
			if strings.EqualFold(name, source.Type.Name) {
				return id
			}
		}
	}
	return c.JiraIssueType
}
