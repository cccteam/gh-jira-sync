package ghjira

import (
	"strings"
	"testing"
)

// contentSyncer links the fake pair with content sync on; the Jira copy holds what
// creation wrote unless the test changes it.
func contentSyncer(t *testing.T) (*syncer, *State) {
	t.Helper()
	x := newSyncer(t)
	x.c.SyncContent, x.c.Comments = true, false
	s := x.linked()
	x.api.jira.Fields.Summary = x.api.source.Title
	x.api.jira.Fields.Description = decodeJSON(canonical(markdownADF(issueDescription(&x.api.source)), false))
	return x, s
}

func contentEntries(entries []*Entry) []*Entry {
	var out []*Entry
	for _, e := range entries {
		if e.Field != "" {
			out = append(out, e)
		}
	}
	return out
}

func contentKinds(entries []*Entry) []Kind { return kinds(contentEntries(entries)) }

func TestContentConfig(t *testing.T) {
	t.Parallel()
	eq(t, testConfig(t, nil).SyncContent, false)
	eq(t, testConfig(t, Object{"sync_content": true}).SyncContent, true)
	raw := Object{
		"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
		"jira_project": "ENG", "jira_issue_type": "10001", "sync_content": "yes",
	}
	if _, err := ParseConfig(canonical(raw, false)); err == nil {
		t.Error("non-boolean sync_content accepted")
	}
}

func TestContentCopiesOriginalEdits(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)

	// A copy that matches is recorded without writing, and then stays quiet.
	s, entries := x.ok(s)
	eq(t, contentKinds(entries), []Kind{KindContentAgreed, KindContentAgreed})
	s, entries = x.ok(s)
	eq(t, len(contentEntries(entries)), 0)

	x.api.source.Title, x.api.source.Body = "Renamed", "New **body**\r\nline two"
	s, entries = x.ok(s)
	eq(t, contentKinds(entries), []Kind{KindSyncContent, KindSyncContent})
	eq(t, contentEntries(entries)[0].Destination, SideJira)
	eq(t, x.api.jira.Fields.Summary, "Renamed")
	if !sameJSON(x.api.jira.Fields.Description, markdownADF(issueDescription(&x.api.source))) {
		t.Errorf("Jira description = %s", canonical(x.api.jira.Fields.Description, false))
	}

	// What was written is recorded, so the next run does nothing.
	x.api.writes = nil
	_, entries = x.ok(s)
	eq(t, len(contentEntries(entries)), 0)
	eq(t, len(x.api.writes), 0)
}

func TestContentNeverOverwritesEditedCopy(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)
	s, _ = x.ok(s)

	x.api.jira.Fields.Summary = "Edited in Jira"
	x.api.source.Title = "Edited on GitHub"
	x.api.writes = nil
	s, entries := x.ok(s)
	got := contentEntries(entries)
	eq(t, kinds(got), []Kind{KindBlocked})
	if !strings.Contains(got[0].Reason, "Jira summary was edited") {
		t.Errorf("reason = %q", got[0].Reason)
	}
	eq(t, len(x.api.writes), 0)
	eq(t, x.api.jira.Fields.Summary, "Edited in Jira")

	// Making the same change on the original records the match; later edits copy again.
	x.api.source.Title = "Edited in Jira"
	s, entries = x.ok(s)
	eq(t, contentKinds(entries), []Kind{KindContentAgreed})
	x.api.source.Title = "Final"
	_, entries = x.ok(s)
	eq(t, contentKinds(entries), []Kind{KindSyncContent})
	eq(t, x.api.jira.Fields.Summary, "Final")
}

func TestContentFirstSyncReplacesDifferingCopy(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)
	x.api.source.Body = "Edited before content sync was on"
	_, entries := x.ok(s)
	eq(t, contentKinds(entries), []Kind{KindContentAgreed, KindSyncContent})
	eq(t, contentEntries(entries)[1].Field, fieldDescription)
	eq(t, x.api.writes[len(x.api.writes)-1].kind, "jira_description")
}

func TestContentRefusedEditRetries(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)
	s, _ = x.ok(s)
	x.api.source.Title = "Renamed"
	x.api.refuse = []int{400}
	s, entries := x.ok(s)
	got := contentEntries(entries)
	eq(t, []any{got[0].Kind, got[0].Outcome}, []any{KindSyncContent, OutcomeRejected})
	eq(t, x.api.jira.Fields.Summary, "Original title")
	_, entries = x.ok(s)
	eq(t, contentKinds(entries), []Kind{KindSyncContent})
	eq(t, x.api.jira.Fields.Summary, "Renamed")
}

func TestContentFromJiraOriginal(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)
	pair := s.Pairs["1"]
	op := token(x.c, OpJiraIssue, pair.JiraID)
	s.Operations[op] = Operation{Kind: OpJiraIssue, SourceID: pair.JiraID, GitHub: 1, Status: StatusDone}
	x.api.jira.Fields.Description = toADF("From Jira")
	body, err := githubBody(x.c, x.api.jira, op)
	must(t, err)
	x.api.source.Body = body
	s, entries := x.ok(s)
	eq(t, contentKinds(entries), []Kind{KindContentAgreed, KindContentAgreed})

	x.api.jira.Fields.Summary = "Renamed in Jira"
	x.api.jira.Fields.Description = toADF("Changed in Jira")
	s, entries = x.ok(s)
	eq(t, contentKinds(entries), []Kind{KindSyncContent, KindSyncContent})
	eq(t, contentEntries(entries)[0].Destination, SideGitHub)
	eq(t, x.api.source.Title, "Renamed in Jira")
	if !strings.Contains(x.api.source.Body, "Changed in Jira") || !strings.HasSuffix(x.api.source.Body, hiddenMarker(op)) {
		t.Errorf("GitHub body = %q", x.api.source.Body)
	}

	x.api.source.Title = "Renamed on GitHub"
	_, entries = x.ok(s)
	got := contentEntries(entries)
	eq(t, kinds(got), []Kind{KindBlocked})
	if !strings.Contains(got[0].Reason, "GitHub title was edited") {
		t.Errorf("reason = %q", got[0].Reason)
	}
}
