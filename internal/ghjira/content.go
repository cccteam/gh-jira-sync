package ghjira

// Content sync (sync_content) copies later title and description edits from the issue
// a pair started on, its original, to the other issue, its copy. Each pair records
// digests of both sides as they stood when a field was last copied or found matching.
// A copy that no longer matches its record was edited there. A title edit there is
// copied back, since titles are plain text on both sides; a description edit is
// reported and never copied back or overwritten, since converting Jira's rich text to
// Markdown loses formatting. The first time a field is synced, a copy that already
// shows the original's text is recorded as matching; any other copy is replaced from
// the original.

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
)

const (
	fieldTitle       = "title"
	fieldDescription = "description"
)

// fieldValues is one content field on both sides of a pair.
type fieldValues struct {
	origin   Side   // the side the pair started on
	original any    // the original's value
	copy     any    // the copy's value as read
	want     any    // what the copy should hold, rendered from the original
	problem  string // why want cannot be written, or ""
}

// originSide is the side a pair started on: Jira for GitHub issues created from Jira
// issues, otherwise GitHub.
func originSide(c *Config, s *State, pair Pair) Side {
	if r, ok := s.Operations[token(c, OpJiraIssue, pair.JiraID)]; ok && r.Kind == OpJiraIssue {
		return SideJira
	}

	return SideGitHub
}

// readValues reads one field from both issues and renders what the copy should hold,
// the same way creation rendered it.
func readValues(c *Config, origin Side, field string, pair Pair, source *GitHubIssue, jira *JiraIssue) fieldValues {
	v := fieldValues{origin: origin}
	switch {
	case field == fieldTitle && origin == SideGitHub:
		v.original, v.copy, v.want = source.Title, jira.Fields.Summary, source.Title
		v.problem = titleProblem(SideJira, source.Title)
	case field == fieldTitle:
		v.original, v.copy, v.want = jira.Fields.Summary, source.Title, jira.Fields.Summary
		v.problem = titleProblem(SideGitHub, jira.Fields.Summary)
	case origin == SideGitHub:
		text := issueDescription(source)
		v.original, v.copy, v.want = normalized(source.Body), jira.Fields.Description, markdownADF(text)
		if runeLen(text) > textLimit {
			v.problem = "Issue description exceeds conservative 30000 character limit"
		}
	default:
		body, err := githubBody(c, jira, token(c, OpJiraIssue, pair.JiraID))
		v.original, v.copy, v.want = jira.Fields.Description, source.Body, body
		switch {
		case err != nil:
			v.problem = err.Error()
		case runeLen(body) > 65536:
			v.problem = "Jira description exceeds GitHub's 65536 character body limit"
		}
	}

	return v
}

// titleProblem says why a title cannot be written to dest, or "".
func titleProblem(dest Side, title string) string {
	switch {
	case dest == SideJira && runeLen(title) > 255:
		return "GitHub title exceeds Jira summary limit"
	case dest == SideJira:
		return ""
	case strings.TrimSpace(title) == "":
		return "Jira summary is empty"
	case runeLen(title) > 256:
		return "Jira summary exceeds GitHub's 256 character title limit"
	}

	return ""
}

// exact reports a copy that already holds exactly what would be written.
func (v fieldValues) exact() bool {
	if text, ok := v.copy.(string); ok {
		return normalized(text) == v.want
	}

	return sameJSON(v.copy, v.want)
}

// shown reports a copy that shows the original's text: exactly, or apart from
// surrounding spaces and formatting that Jira may store differently.
func (v fieldValues) shown() bool {
	if v.exact() {
		return true
	}
	if text, ok := v.copy.(string); ok {
		want, _ := v.want.(string)

		return strings.TrimSpace(normalized(text)) == strings.TrimSpace(want)
	}
	got, err := descriptionText(v.copy)
	if err != nil {
		return false
	}
	want, err := descriptionText(v.want)

	return err == nil && got == want
}

// planContent plans the title and description of a linked pair.
func planContent(c *Config, s *State, api API, pair Pair, source *GitHubIssue, jira *JiraIssue) ([]*Entry, error) {
	origin := originSide(c, s, pair)
	entries := make([]*Entry, 0, 2)
	for _, f := range []struct {
		name   string
		op     OpKind
		record *Copied
	}{{fieldTitle, OpTitle, pair.Title}, {fieldDescription, OpDescription, pair.Description}} {
		v := readValues(c, origin, f.name, pair, source, jira)
		e := &Entry{
			GitHub: pair.GitHub, Operation: token(c, f.op, pair.GitHubID), Field: f.name,
			pair: pair, issue: source, jira: jira, content: v,
		}
		edited := f.record != nil && digest(v.copy) != f.record.Copy
		changed := f.record == nil || digest(v.original) != f.record.Original
		switch {
		case !edited && !changed:
			continue
		// An edited copy that shows the original again, for example after the same change
		// was made there, is recorded; so is a first look at a copy that matches.
		case v.exact() || ((f.record == nil || edited) && v.shown()):
			e.Kind = KindContentAgreed
		case edited && f.name == fieldTitle:
			if err := planTitleEdit(api, e, changed); err != nil {
				return nil, err
			}
		case edited:
			e.block(editedReason(origin, f.name))
		case v.problem != "":
			e.block(v.problem)
		default:
			e.Kind, e.Destination = KindSyncContent, origin.Other()
		}
		entries = append(entries, e)
	}

	return entries, nil
}

// planTitleEdit plans a title edited on the copy: it is copied back to the original.
// If the original's title was edited too, the later edit wins, by GitHub's last rename
// and the last summary change in Jira's changelog (each issue's creation if none).
func planTitleEdit(api API, e *Entry, changed bool) error {
	v := e.content
	from := v.origin.Other() // the side whose title is copied
	if changed {
		githubAt, err := api.GitHubTitleChanged(e.GitHub)
		if err != nil {
			return err
		}
		jiraAt, err := api.JiraFieldChanged(e.pair.JiraID, "summary")
		if err != nil {
			return err
		}
		later, why := laterSide(cmp.Or(githubAt, e.issue.CreatedAt), cmp.Or(jiraAt, e.jira.Fields.Created))
		if later == "" {
			e.block("Title edited on both GitHub and Jira; " + why + ", so make them match by hand")

			return nil
		}
		from = later
	}
	title, _ := v.copy.(string)
	if from == v.origin {
		title, _ = v.original.(string)
	}
	if problem := titleProblem(from.Other(), title); problem != "" {
		e.block(problem)

		return nil
	}
	e.content.want = title
	e.Kind, e.Destination = KindSyncContent, from.Other()
	e.Reason = serviceName(from) + " " + fieldName(from, fieldTitle) + " was edited last; copied to " + serviceName(from.Other())

	return nil
}

// fieldName is a content field as the given side calls it.
func fieldName(side Side, field string) string {
	switch {
	case field == fieldDescription:
		return fieldDescription
	case side == SideJira:
		return "summary"
	default:
		return fieldTitle
	}
}

func serviceName(side Side) string {
	if side == SideJira {
		return "Jira"
	}

	return "GitHub"
}

func editedReason(origin Side, field string) string {
	copied := origin.Other()

	return fmt.Sprintf("%s %s was edited on the copy, and edits there are never copied back or overwritten; "+
		"make the same change on %s, where the issue started, or undo it on %s",
		serviceName(copied), fieldName(copied, field), serviceName(origin), serviceName(copied))
}

// setCopied records a content entry's field on both sides as matching.
func setCopied(s *State, e *Entry) {
	n := strconv.Itoa(e.GitHub)
	pair := s.Pairs[n]
	record := &Copied{Original: digest(e.content.original), Copy: digest(e.content.copy)}
	if e.Field == fieldTitle {
		pair.Title = record
	} else {
		pair.Description = record
	}
	s.Pairs[n] = pair
}

// applyContent copies one field from the original to the copy, re-reading both sides
// before and after. Like status changes it is idempotent and not journaled.
func applyContent(c *Config, s *State, api API, e *Entry, save func(*State) error) error {
	read := func() (fieldValues, error) {
		source, err := api.Issue(e.GitHub)
		if err != nil {
			return fieldValues{}, err
		}
		jira, err := api.JiraIssue(e.pair.JiraID)
		if err != nil {
			return fieldValues{}, err
		}
		if source == nil || jira == nil || source.idString() != e.pair.GitHubID {
			return fieldValues{}, syncErr("Linked issue vanished after plan; rerun")
		}

		return readValues(c, e.content.origin, e.Field, e.pair, source, jira), nil
	}
	now, err := read()
	if err != nil {
		return err
	}
	if !sameJSON(now.original, e.content.original) || !sameJSON(now.copy, e.content.copy) {
		return syncErr("Issue %s changed after plan; rerun", e.Field)
	}
	name := fieldName(e.Destination, e.Field)
	if e.Destination == SideGitHub {
		if e.Field == fieldDescription {
			name = "body"
		}
		text, _ := e.content.want.(string)
		err = api.EditGitHubIssue(e.GitHub, name, text)
	} else {
		err = api.EditJiraIssue(e.pair.JiraID, Object{name: e.content.want})
	}
	if status, refused := refusedStatus(err); refused && itemRejections[status] {
		// For example a Jira field missing from the edit screen; nothing changed.
		e.Outcome, e.Reason = OutcomeRejected, serviceName(e.Destination)+" "+name+
			" change rejected with HTTP "+strconv.Itoa(status)+"; retried next run"

		return nil
	}
	if err != nil {
		return err
	}
	if now, err = read(); err != nil {
		return err
	}
	if !now.shown() {
		return syncErr("%s %s change not confirmed; the copy does not show the original", serviceName(e.Destination), name)
	}
	e.content = now
	setCopied(s, e)

	return save(s)
}
