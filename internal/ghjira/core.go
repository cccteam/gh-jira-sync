// Package ghjira creates Jira issues from GitHub issues and copies comments both ways.
//
// Configuration, stable operation identities, and conservative text conversion live here.
package ghjira

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// SyncError is an expected, reportable failure; its message is shown verbatim.
type SyncError struct {
	Msg string
	// rerun marks an entry that went stale between plan and apply before anything was
	// recorded for it: apply skips the entry, and the next run plans it again.
	rerun bool
}

func (e *SyncError) Error() string { return e.Msg }

func syncErr(format string, args ...any) error {
	return &SyncError{Msg: fmt.Sprintf(format, args...)}
}

// rerunErr is a SyncError that fails only its own entry; see SyncError.rerun.
func rerunErr(format string, args ...any) error {
	return &SyncError{Msg: fmt.Sprintf(format, args...), rerun: true}
}

// isRerun reports an error that should skip its entry rather than stop the run.
func isRerun(err error) bool {
	var s *SyncError

	return errors.As(err, &s) && s.rerun
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
	Repository string
	GitHubURL  string
	JiraURL    string
	// JiraCloudID routes Jira API calls through Atlassian's API gateway, which
	// Atlassian service account tokens require; "" calls JiraURL directly. Links
	// in copies and the deployment binding always use JiraURL.
	JiraCloudID string
	JiraProject string
	// JiraDefaultIssueType, the values of JiraIssueTypes, and JiraSubtaskType name a
	// Jira issue type by ID (digits) or by name (ignoring case); CheckProject replaces
	// names with IDs.
	JiraDefaultIssueType string
	// JiraIssueTypes maps GitHub issue type names (matched ignoring case) to Jira
	// issue types for new issues; others use JiraDefaultIssueType.
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
	// SyncContent copies later title and description edits from the issue a pair
	// started on to its copy. Title edits on the copy are copied back too; description
	// edits on the copy are blocked for review, never copied back.
	SyncContent bool
	// JiraSubtaskType is the Jira sub-task issue type for new issues whose GitHub
	// parent is a standard Jira issue (such as a Task under a Task); "" turns this off.
	JiraSubtaskType string
	// JiraSelectionLabel selects Jira issues in the project to create in GitHub; nil
	// disables Jira → GitHub creation.
	JiraSelectionLabel *string
	ProjectStatus      *ProjectStatus // nil disables project status mode
	// JiraOwnedIssueTypes lists Jira issue types (IDs after CheckProject) that Jira
	// alone decides: their open/closed state, project status, and parent only flow
	// Jira → GitHub, GitHub-side changes are set back to Jira's, and GitHub issues of
	// these types are never created in Jira.
	JiraOwnedIssueTypes []string
	// IgnoredJiraParents lists Jira issue keys, such as an initiative in another
	// project, whose children skip parent sync instead of blocking for review.
	IgnoredJiraParents []string
	// AssigneeMap maps GitHub logins to Jira account IDs, one to one; nil turns
	// assignee sync off. People missing from it are left alone on both sides.
	AssigneeMap map[string]string
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
}

// ProjectStatusMapping pairs one project option with the Jira statuses it covers.
// Jira[0] is the status Jira moves to; Transition picks one when several lead there.
type ProjectStatusMapping struct {
	Project    string
	Jira       []string
	Transition string
}

func (p *ProjectStatus) mapping(option string) (ProjectStatusMapping, bool) {
	for _, m := range p.Map {
		if m.Project == option {
			return m, true
		}
	}

	return ProjectStatusMapping{}, false
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

// jiraStatuses reads a map entry's "jira": one status name or a list; the first listed
// is the one Jira moves to.
func jiraStatuses(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		names := make([]string, 0, len(v))
		for _, s := range v {
			name, _ := s.(string)
			names = append(names, name)
		}

		return names
	default:
		return nil
	}
}

// parseStatusMapping reads one project_status map entry, recording its option and
// statuses as seen. It returns a problem description instead of the mapping when the
// entry is invalid.
func parseStatusMapping(entry any, seenOption, seenJira map[string]bool) (m ProjectStatusMapping, problem string) {
	e, ok := entry.(Object)
	if !ok {
		return m, "each map entry must be an object"
	}
	for k := range e {
		switch {
		case k == "jira_backlog":
			return m, backlogModeRemoved
		case k != "project" && k != "jira" && k != "transition":
			return m, "unknown map key " + strconv.Quote(k)
		}
	}
	if m.Project, _ = e["project"].(string); strings.TrimSpace(m.Project) == "" || seenOption[m.Project] {
		return m, "each map entry needs a distinct project option name"
	}
	seenOption[m.Project] = true
	if m.Jira = jiraStatuses(e["jira"]); len(m.Jira) == 0 {
		return m, "map entry " + strconv.Quote(m.Project) + " needs at least one Jira status"
	}
	for _, name := range m.Jira {
		if strings.TrimSpace(name) == "" || seenJira[name] {
			return m, "Jira statuses must be non-empty and appear in only one map entry"
		}
		seenJira[name] = true
	}
	if v, present := e["transition"]; present {
		if m.Transition, _ = v.(string); strings.TrimSpace(m.Transition) == "" {
			return m, "transition must be a transition name"
		}
	}

	return m, ""
}

// backlogModeRemoved explains configurations written for the board backlog mode that
// earlier releases had, which tracked backlog membership apart from status.
const backlogModeRemoved = "jira_board and jira_backlog were removed; " +
	"map a Jira Backlog status instead, after moving backlog issues into it"

func parseProjectStatus(raw any) (*ProjectStatus, error) {
	bad := func(detail string) (*ProjectStatus, error) {
		return nil, syncErr("project_status: %s", detail)
	}
	obj, ok := raw.(Object)
	if !ok {
		return bad("must be null or an object")
	}
	allowed := map[string]bool{"owner": true, "owner_type": true, "project_number": true, "field": true, "add_to_project": true, "map": true}
	for k := range obj {
		if k == "jira_board" {
			return bad(backlogModeRemoved)
		}
		if !allowed[k] {
			return bad("unknown key " + strconv.Quote(k))
		}
	}
	p := &ProjectStatus{OwnerType: "org", Field: "Status", AddToProject: true}
	if p.Owner, _ = obj["owner"].(string); !ownerPattern.MatchString(p.Owner) {
		return bad("owner must be a GitHub organization or user name")
	}
	if v, present := obj["owner_type"]; present {
		if p.OwnerType, _ = v.(string); p.OwnerType != "org" && p.OwnerType != ownerUser {
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
	for _, entry := range entries {
		m, problem := parseStatusMapping(entry, seenOption, seenJira)
		if problem != "" {
			return bad(problem)
		}
		p.Map = append(p.Map, m)
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
	cloudIDPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	projectPattern    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	// markerPattern is the visible marker older releases appended to copies;
	// hiddenMarkerPattern is the HTML comment GitHub copies now end with.
	markerPattern       = regexp.MustCompile(`^\[gh-jira-sync:([a-f0-9]{64})\]$`)
	hiddenMarkerPattern = regexp.MustCompile(`^<!--\s*gh-jira-sync:([a-f0-9]{64})\s*-->$`)
)

// legacyIssueTypeKey is jira_default_issue_type's name in earlier releases, still
// accepted so existing configurations keep working. It is not in configKeys, which
// lists the current options.
const legacyIssueTypeKey = "jira_issue_type"

var configKeys = map[string]bool{
	"repository": true, "github_url": true, "jira_url": true, "jira_project": true,
	"jira_default_issue_type": true, "selection_label": true, "issue_state": true, "max_operations": true,
	"max_pages": true, "comments": true, "comments_since": true, "jira_create_fields": true,
	"full_rescan_hours": true, "status_sync": true, "project_status": true, "jira_issue_types": true,
	"sync_parents": true, "sync_content": true, "jira_selection_label": true, "jira_subtask_type": true, "jira_cloud_id": true,
	"jira_owned_issue_types": true, "ignored_jira_parents": true, "assignee_map": true,
}

// jiraParentField is the Jira issue field that holds an issue's parent.
const jiraParentField = "parent"

// jiraGateway is Atlassian's API gateway; a site's API lives at jiraGateway + cloud ID.
const jiraGateway = "https://api.atlassian.com/ex/jira/"

// JiraAPIURL is the base URL for Jira API calls: the gateway when JiraCloudID is
// set, otherwise the site itself.
func (c *Config) JiraAPIURL() string {
	if c.JiraCloudID != "" {
		return jiraGateway + c.JiraCloudID
	}

	return c.JiraURL
}

// ParseConfig validates sync.json. Unknown keys and wrong types fail closed.
func ParseConfig(data []byte) (*Config, error) {
	var top any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, syncErr("Configuration must be a single JSON object")
	}
	raw, ok := top.(Object)
	if !ok {
		return nil, syncErr("Configuration must be a JSON object")
	}
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		if !configKeys[k] && k != legacyIssueTypeKey {
			return nil, syncErr("Unknown configuration key %q", k)
		}
	}
	c := &Config{}
	for _, step := range []func(configInput, *Config) error{
		parseEndpoints, parseIssueTypes, parseSelection, parseLimits, parseToggles, parseCreateFields, parseStatusModes, parseOwnership, parseAssignees,
	} {
		if err := step(configInput(raw), c); err != nil {
			return nil, err
		}
	}

	return c, nil
}

// configInput is the decoded sync.json object.
type configInput Object

// value returns the key's value, or fallback when the key is absent.
func (in configInput) value(key string, fallback any) any {
	if v, present := in[key]; present {
		return v
	}

	return fallback
}

// parseEndpoints reads the repository, the GitHub and Jira URLs, and the Jira project.
func parseEndpoints(in configInput, c *Config) error {
	var str bool
	if c.Repository, str = in["repository"].(string); !str || !repositoryPattern.MatchString(c.Repository) {
		return syncErr("repository must be owner/name")
	}
	for _, key := range []string{"github_url", "jira_url"} {
		fallback := any(nil)
		if key == "github_url" {
			fallback = "https://api.github.com"
		}
		s, isString := in.value(key, fallback).(string)
		if !isString {
			return syncErr("%s is required", key)
		}
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return syncErr("%s must be an HTTPS URL without credentials, query, or fragment", key)
		}
		if key == "github_url" {
			c.GitHubURL = strings.TrimRight(s, "/")
		} else {
			c.JiraURL = strings.TrimRight(s, "/")
		}
	}
	if c.JiraProject, str = in["jira_project"].(string); !str || !projectPattern.MatchString(c.JiraProject) {
		return syncErr("jira_project must be a project key")
	}
	switch id := in.value("jira_cloud_id", nil).(type) {
	case nil:
	case string:
		if c.JiraCloudID = strings.ToLower(id); !cloudIDPattern.MatchString(c.JiraCloudID) {
			return syncErr("jira_cloud_id must be the site's cloud ID, a UUID")
		}
	default:
		return syncErr("jira_cloud_id must be a string or null")
	}

	return nil
}

// parseIssueTypes reads the default Jira issue type and the per-GitHub-type mapping.
func parseIssueTypes(in configInput, c *Config) error {
	key := "jira_default_issue_type"
	if _, legacy := in[legacyIssueTypeKey]; legacy {
		if _, current := in[key]; current {
			return syncErr("Set jira_default_issue_type only; jira_issue_type is its old name")
		}
		key = legacyIssueTypeKey
	}
	var str bool
	if c.JiraDefaultIssueType, str = in[key].(string); !str || strings.TrimSpace(c.JiraDefaultIssueType) == "" {
		return syncErr("jira_default_issue_type must be an issue type ID or name")
	}
	c.JiraIssueTypes = map[string]string{}
	raw, present := in["jira_issue_types"]
	if !present || raw == nil {
		return nil
	}
	types, ok := raw.(Object)
	if !ok {
		return syncErr("jira_issue_types must map GitHub issue type names to Jira issue type IDs or names")
	}
	// Names match GitHub issue types ignoring case, so two that differ only in case
	// would be ambiguous.
	accepted := make([]string, 0, len(types))
	for name, id := range types {
		idString, _ := id.(string)
		if strings.TrimSpace(name) == "" || strings.TrimSpace(idString) == "" {
			return syncErr("jira_issue_types must map GitHub issue type names to Jira issue type IDs or names")
		}
		if slices.ContainsFunc(accepted, func(other string) bool { return strings.EqualFold(other, name) }) {
			return syncErr("jira_issue_types lists %q twice (names ignore case)", name)
		}
		accepted = append(accepted, name)
		c.JiraIssueTypes[name] = idString
	}

	return nil
}

// parseSelection reads which GitHub issues are created in Jira.
func parseSelection(in configInput, c *Config) error {
	switch label := in.value("selection_label", "sync-jira").(type) {
	case nil:
	case string:
		if strings.TrimFunc(label, isPythonSpace) == "" || strings.Contains(label, ",") {
			return syncErr("selection_label must be one label or null (all issues)")
		}
		c.SelectionLabel = &label
	default:
		return syncErr("selection_label must be one label or null (all issues)")
	}
	c.IssueState, _ = in.value("issue_state", stateOpen).(string)
	if c.IssueState != stateOpen && c.IssueState != stateClosed && c.IssueState != "all" {
		return syncErr("issue_state must be open, closed, or all")
	}

	return nil
}

// parseLimits reads the positive integer limits.
func parseLimits(in configInput, c *Config) error {
	for _, field := range []struct {
		key      string
		fallback int
		target   *int
	}{{"max_operations", 20, &c.MaxOperations}, {"max_pages", 100, &c.MaxPages}, {"full_rescan_hours", 24, &c.FullRescanHours}} {
		n, ok := strictInt(in.value(field.key, json.Number(strconv.Itoa(field.fallback))))
		if !ok || n < 1 {
			return syncErr("%s must be a positive integer", field.key)
		}
		*field.target = n
	}

	return nil
}

// parseToggles reads comment and parent sync, sub-tasks, Jira → GitHub creation, and
// the comment cutoff.
func parseToggles(in configInput, c *Config) error {
	var ok bool
	if c.Comments, ok = in.value("comments", true).(bool); !ok {
		return syncErr("comments must be boolean")
	}
	if c.SyncParents, ok = in.value("sync_parents", false).(bool); !ok {
		return syncErr("sync_parents must be boolean")
	}
	if c.SyncContent, ok = in.value("sync_content", false).(bool); !ok {
		return syncErr("sync_content must be boolean")
	}
	switch id := in.value("jira_subtask_type", nil).(type) {
	case nil:
	case string:
		if strings.TrimSpace(id) == "" {
			return syncErr("jira_subtask_type must be a sub-task issue type ID or name, or null")
		}
		// A sub-task's parent is fixed at creation and then kept by parent sync.
		if !c.SyncParents {
			return syncErr("jira_subtask_type needs sync_parents")
		}
		c.JiraSubtaskType = id
	default:
		return syncErr("jira_subtask_type must be a sub-task issue type ID or name, or null")
	}
	switch label := in.value("jira_selection_label", nil).(type) {
	case nil:
	case string:
		// Jira labels cannot contain spaces; quotes would break the JQL that selects them.
		if label == "" || strings.ContainsAny(label, " \"\\") {
			return syncErr("jira_selection_label must be one Jira label (no spaces or quotes) or null")
		}
		c.JiraSelectionLabel = &label
	default:
		return syncErr("jira_selection_label must be one Jira label (no spaces or quotes) or null")
	}
	if since := in.value("comments_since", nil); since != nil {
		s, isString := since.(string)
		t, err := parseTimestamp(s)
		if !isString || err != nil {
			return syncErr("comments_since must be an ISO timestamp with timezone")
		}
		c.CommentsSince = &t
	}

	return nil
}

// parseCreateFields reads extra fields for new Jira issues, which may not replace the
// ones the sync sets.
func parseCreateFields(in configInput, c *Config) error {
	extra, ok := in.value("jira_create_fields", Object{}).(Object)
	if !ok {
		return syncErr("jira_create_fields must be an object of Jira fields")
	}
	// parent is set per issue for sub-tasks and kept by parent sync, so a fixed one
	// would put every new issue under the same parent.
	for _, k := range []string{"summary", "description", "project", "issuetype", jiraParentField} {
		if _, present := extra[k]; present {
			return syncErr("jira_create_fields cannot override project, issue type, summary, description, or parent")
		}
	}
	c.JiraCreateFields = extra

	return nil
}

// parseStatusModes reads status_sync or project_status, which exclude each other.
func parseStatusModes(in configInput, c *Config) error {
	if raw, present := in["status_sync"]; present && raw != nil {
		statusSync, ok := raw.(Object)
		closeName, _ := statusSync["close_transition"].(string)
		reopenName, _ := statusSync["reopen_transition"].(string)
		if !ok || len(statusSync) != 2 || strings.TrimSpace(closeName) == "" || strings.TrimSpace(reopenName) == "" {
			return syncErr("status_sync must be null or {\"close_transition\": name, \"reopen_transition\": name}")
		}
		c.StatusSync = &StatusSync{CloseTransition: closeName, ReopenTransition: reopenName}
	}
	if raw, present := in["project_status"]; present && raw != nil {
		if c.StatusSync != nil {
			// Both would move Jira when a GitHub issue closes, possibly to different statuses.
			return syncErr("Use either status_sync or project_status, not both")
		}
		ps, err := parseProjectStatus(raw)
		if err != nil {
			return err
		}
		c.ProjectStatus = ps
	}

	return nil
}

// jiraKeyPattern matches a Jira issue key such as ENG-12.
var jiraKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[1-9]\d*$`)

// distinctStrings reads an optional JSON list of distinct non-empty strings.
func distinctStrings(in configInput, key string, valid func(string) bool, fold bool) ([]string, bool) {
	raw := in.value(key, nil)
	if raw == nil {
		return nil, true
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	list := make([]string, 0, len(items))
	for _, item := range items {
		s, isString := item.(string)
		if !isString || !valid(s) || slices.ContainsFunc(list, func(other string) bool {
			return other == s || (fold && strings.EqualFold(other, s))
		}) {
			return nil, false
		}
		list = append(list, s)
	}

	return list, true
}

// parseOwnership reads the Jira-owned issue types and the ignored Jira parents.
func parseOwnership(in configInput, c *Config) error {
	var ok bool
	nonEmpty := func(s string) bool { return strings.TrimSpace(s) != "" }
	if c.JiraOwnedIssueTypes, ok = distinctStrings(in, "jira_owned_issue_types", nonEmpty, true); !ok {
		return syncErr("jira_owned_issue_types must be a list of distinct Jira issue type IDs or names")
	}
	if len(c.JiraOwnedIssueTypes) > 0 && c.StatusSync == nil && c.ProjectStatus == nil && !c.SyncParents {
		return syncErr("jira_owned_issue_types needs status_sync, project_status, or sync_parents")
	}
	if c.IgnoredJiraParents, ok = distinctStrings(in, "ignored_jira_parents", jiraKeyPattern.MatchString, false); !ok {
		return syncErr("ignored_jira_parents must be a list of distinct Jira issue keys, such as ENG-12")
	}
	if len(c.IgnoredJiraParents) > 0 && !c.SyncParents {
		return syncErr("ignored_jira_parents needs sync_parents")
	}

	return nil
}

// jiraAssigneeField is the Jira issue field that holds an issue's assignee.
const jiraAssigneeField = "assignee"

// parseAssignees reads assignee_map. Each GitHub login (matched ignoring case, as
// GitHub does) maps to one Jira account ID, and no account ID is mapped twice, so
// either side's assignee names exactly one person on the other.
func parseAssignees(in configInput, c *Config) error {
	raw := in.value("assignee_map", nil)
	if raw == nil {
		return nil
	}
	const shape = "assignee_map must map GitHub logins to distinct Jira account IDs, or be null"
	people, ok := raw.(Object)
	if !ok || len(people) == 0 {
		return syncErr(shape)
	}
	c.AssigneeMap = make(map[string]string, len(people))
	for _, login := range slices.Sorted(maps.Keys(people)) {
		account, _ := people[login].(string)
		if !githubLoginPattern.MatchString(login) || strings.TrimSpace(account) == "" || account != strings.TrimSpace(account) {
			return syncErr(shape)
		}
		for other, otherAccount := range c.AssigneeMap {
			if strings.EqualFold(other, login) {
				return syncErr("assignee_map lists %q twice (logins ignore case)", login)
			}
			if otherAccount == account {
				return syncErr("assignee_map maps %q and %q to the same Jira account", other, login)
			}
		}
		c.AssigneeMap[login] = account
	}
	// Assignee sync sets a new issue's assignee itself; a fixed one would assign
	// every new issue to the same person.
	if _, present := c.JiraCreateFields[jiraAssigneeField]; present {
		return syncErr("jira_create_fields cannot set assignee when assignee_map is set")
	}

	return nil
}

// githubLoginPattern matches a GitHub login: letters, digits, hyphens, and the
// underscore that Enterprise Managed User logins put before their short code (as in
// octocat_acme). It only catches typos such as spaces or a leading @; GitHub decides
// whether a login can be assigned.
var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// jiraAccount returns the Jira account ID mapped to a GitHub login, if any.
func (c *Config) jiraAccount(login string) (string, bool) {
	for l, account := range c.AssigneeMap {
		if strings.EqualFold(l, login) {
			return account, true
		}
	}

	return "", false
}

// githubLogin returns the GitHub login mapped to a Jira account ID, if any.
func (c *Config) githubLogin(account string) (string, bool) {
	for login, a := range c.AssigneeMap {
		if a == account {
			return login, true
		}
	}

	return "", false
}

// assignedAccounts returns the Jira account IDs of an issue's mapped GitHub
// assignees, in GitHub's order, and the logins of its unmapped ones.
func (c *Config) assignedAccounts(issue *GitHubIssue) (mapped, unmapped []string) {
	for _, u := range issue.Assignees {
		if account, ok := c.jiraAccount(u.Login); ok {
			mapped = append(mapped, account)
		} else {
			unmapped = append(unmapped, u.Login)
		}
	}

	return mapped, unmapped
}

// newAssignee is the Jira account a new issue is created with: its first mapped
// GitHub assignee, or "" when assignee sync is off or it has none.
func (c *Config) newAssignee(source *GitHubIssue) string {
	if c.AssigneeMap == nil {
		return ""
	}
	mapped, _ := c.assignedAccounts(source)
	if len(mapped) == 0 {
		return ""
	}

	return mapped[0]
}

// jiraOwned reports whether Jira alone decides this Jira issue's status and parent.
func (c *Config) jiraOwned(j *JiraIssue) bool {
	return j != nil && slices.Contains(c.JiraOwnedIssueTypes, j.Fields.IssueType.ID)
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
	return digest(Object{
		"repository": c.Repository, "github_url": c.GitHubURL,
		"jira_url": c.JiraURL, "jira_project": c.JiraProject,
	})
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

var plainMarks = map[string]bool{
	"strong": true, "em": true, "strike": true, "underline": true,
	"code": true, "textColor": true, "backgroundColor": true, "subsup": true,
}

var blockSeparators = map[string]string{
	"doc": "\n", "paragraph": "", "heading": "", "blockquote": "\n",
	"codeBlock": "", "bulletList": "\n", "orderedList": "\n", "listItem": "\n",
	"panel": "\n", "expand": "\n", "nestedExpand": "\n",
	"taskList": "\n", "taskItem": "", "decisionList": "\n", "decisionItem": "",
}

// tableSeparators add the table nodes markdownADF writes into descriptions. Comments
// still block tables, since a table's text alone would lose its layout in a copy.
var tableSeparators = func() map[string]string {
	m := maps.Clone(blockSeparators)
	maps.Copy(m, map[string]string{"table": "\n", "tableRow": " | ", "tableHeader": "\n", "tableCell": "\n"})

	return m
}()

// adfText renders readable plain text and never silently discards unsupported nodes or marks.
func adfText(doc any) (string, error) {
	return renderDoc(doc, blockSeparators)
}

// descriptionText is adfText for comparing descriptions, which may hold tables.
func descriptionText(doc any) (string, error) {
	return renderDoc(doc, tableSeparators)
}

func renderDoc(doc any, separators map[string]string) (string, error) {
	d, ok := doc.(Object)
	if !ok || d["type"] != "doc" || !isOne(d["version"]) {
		return "", syncErr("Unsupported Jira comment document")
	}

	return renderADF(d, separators)
}

func renderADF(node Object, separators map[string]string) (string, error) {
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
	case "inlineCard", "blockCard", "embedCard", "status", "date":
		return renderInlineNode(t, asObject(node["attrs"]))
	}
	separator, block := separators[t]
	if !block {
		return "", unsupportedContent(t)
	}
	var parts []string
	for _, child := range list(node["content"]) {
		c, ok := child.(Object)
		if !ok {
			return "", unsupportedContent("")
		}
		part, err := renderADF(c, separators)
		if err != nil {
			return "", err
		}
		parts = append(parts, part)
	}

	return decorateBlock(t, asObject(node["attrs"]), strings.Join(parts, separator)), nil
}

// decorateBlock adds the marker a block shows in plain text: a list bullet, a task
// checkbox, or an expand's title.
func decorateBlock(t string, attrs Object, text string) string {
	switch t {
	case "listItem", "decisionItem":
		return "- " + text
	case "taskItem":
		if attrs["state"] == "DONE" {
			return "[x] " + text
		}

		return "[ ] " + text
	case "expand", "nestedExpand":
		if title, _ := attrs["title"].(string); title != "" {
			return title + "\n" + text
		}
	}

	return text
}

// renderInlineNode renders Jira smart links, status lozenges, and dates as text.
// Media stays unsupported: an attachment cannot be copied without losing it.
func renderInlineNode(t string, attrs Object) (string, error) {
	switch t {
	case "inlineCard", "blockCard", "embedCard":
		if link, ok := attrs["url"].(string); ok && link != "" {
			return link, nil
		}
	case "status":
		if text, ok := attrs["text"].(string); ok {
			return "[" + text + "]", nil
		}
	case "date":
		stamp, _ := attrs["timestamp"].(string)
		if ms, err := strconv.ParseInt(stamp, 10, 64); err == nil {
			return time.UnixMilli(ms).UTC().Format(time.DateOnly), nil
		}
	}

	return "", unsupportedContent(t)
}

// unsupportedContent names the ADF node type, so the report says what to support next.
func unsupportedContent(t string) error {
	return syncErr("Unsupported Jira comment content (%s); copy manually", cmp.Or(t, "unknown"))
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

func firstRune(s string) (r rune, size int) {
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

// issueTypeFor returns the Jira issue type for a new issue: the one mapped to its
// GitHub issue type, or jira_default_issue_type when it has no type or an unmapped one.
func (c *Config) issueTypeFor(source *GitHubIssue) string {
	if source.Type != nil {
		for name, id := range c.JiraIssueTypes {
			if strings.EqualFold(name, source.Type.Name) {
				return id
			}
		}
	}

	return c.JiraDefaultIssueType
}
