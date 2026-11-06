package ghjira

import (
	"net/http"
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
		"jira_project": "ENG", "jira_default_issue_type": "10001", "sync_content": "yes",
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

func TestContentNeverOverwritesEditedDescription(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)
	s, _ = x.ok(s)

	x.api.jira.Fields.Description = toADF("Edited in Jira")
	x.api.source.Body = "Edited on GitHub"
	x.api.writes = nil
	_, entries := x.ok(s)
	got := contentEntries(entries)
	eq(t, kinds(got), []Kind{KindBlocked})
	if !strings.Contains(got[0].Reason, "Jira description was edited") {
		t.Errorf("reason = %q", got[0].Reason)
	}
	eq(t, len(x.api.writes), 0)
}

func TestContentTitleEditedOnBothSidesWithoutTimesBlocks(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)
	s, _ = x.ok(s)

	x.api.jira.Fields.Summary = "Edited in Jira"
	x.api.source.Title = "Edited on GitHub"
	x.api.writes = nil
	s, entries := x.ok(s)
	got := contentEntries(entries)
	eq(t, kinds(got), []Kind{KindBlocked})
	if !strings.Contains(got[0].Reason, "Title edited on both GitHub and Jira; when each changed is unknown") {
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

// withTableAttrs adds the attributes Jira stores on tables it was sent without them.
func withTableAttrs(doc any) any {
	for _, block := range list(asObject(doc)["content"]) {
		if b := asObject(block); b["type"] == "table" {
			b["attrs"] = Object{"isNumberColumnEnabled": false, "layout": "default", "localId": "a1b2"}
		}
	}
	return doc
}

func TestContentTableDescriptionConfirmed(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)
	x.api.storeADF = withTableAttrs
	s, _ = x.ok(s)

	// Jira stores the table differently from what was sent; it still shows the original.
	x.api.source.Body = "| A | B |\n| --- | --- |\n| 1 | 2 |"
	s, entries := x.ok(s)
	eq(t, contentKinds(entries), []Kind{KindSyncContent})
	if asObject(list(asObject(x.api.jira.Fields.Description)["content"])[1])["attrs"] == nil {
		t.Fatal("fake Jira did not add table attributes")
	}
	x.api.writes = nil
	_, entries = x.ok(s)
	eq(t, len(contentEntries(entries)), 0)
	eq(t, len(x.api.writes), 0)
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

	// A title edited on the GitHub copy is copied back to Jira.
	x.api.source.Title = "Renamed on GitHub"
	_, entries = x.ok(s)
	got := contentEntries(entries)
	eq(t, []any{got[0].Kind, got[0].Destination}, []any{KindSyncContent, SideJira})
	eq(t, x.api.jira.Fields.Summary, "Renamed on GitHub")
}

func TestContentTitleEditedOnCopyIsCopiedBack(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)
	s, _ = x.ok(s)

	x.api.jira.Fields.Summary = "Renamed in Jira"
	s, entries := x.ok(s)
	got := contentEntries(entries)
	eq(t, []any{got[0].Kind, got[0].Destination}, []any{KindSyncContent, SideGitHub})
	if got[0].Reason != "Jira summary was edited last; copied to GitHub" {
		t.Errorf("reason = %q", got[0].Reason)
	}
	eq(t, x.api.source.Title, "Renamed in Jira")

	// Recorded as matching, so nothing is written again.
	x.api.writes = nil
	_, entries = x.ok(s)
	eq(t, len(contentEntries(entries)), 0)
	eq(t, len(x.api.writes), 0)
}

func TestContentTitleEditedOnBothSidesLaterWins(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		github, jira, want string
		dest               Side
	}{
		"Jira later":   {"2026-11-04T10:00:00Z", "2026-11-04T10:05:00.000+0000", "Edited in Jira", SideGitHub},
		"GitHub later": {"2026-11-04T10:05:00Z", "2026-11-04T10:00:00.000+0000", "Edited on GitHub", SideJira},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			x, s := contentSyncer(t)
			s, _ = x.ok(s)
			x.api.jira.Fields.Summary, x.api.summaryChanged = "Edited in Jira", tc.jira
			x.api.source.Title, x.api.titleChanged = "Edited on GitHub", tc.github
			s, entries := x.ok(s)
			eq(t, contentEntries(entries)[0].Destination, tc.dest)
			eq(t, []string{x.api.source.Title, x.api.jira.Fields.Summary}, []string{tc.want, tc.want})
			_, entries = x.ok(s)
			eq(t, len(contentEntries(entries)), 0)
		})
	}
}

func TestContentTitleCopiedBackChecksLimits(t *testing.T) {
	t.Parallel()
	x, s := contentSyncer(t)
	s, _ = x.ok(s)
	x.api.source.Title = "Original title"
	x.api.jira.Fields.Summary = "   "
	_, entries := x.ok(s)
	got := contentEntries(entries)
	eq(t, kinds(got), []Kind{KindBlocked})
	eq(t, got[0].Reason, "Jira summary is empty")
}

func TestGitHubTitleChangedReadsLastRename(t *testing.T) {
	t.Parallel()
	var nodes []any
	srv := newServer(t, func(*http.Request) (int, http.Header, any) {
		return 200, nil, Object{"data": Object{"repository": Object{"issue": Object{"timelineItems": Object{"nodes": nodes}}}}}
	})
	api := srv.client(t)
	got, err := api.GitHubTitleChanged(3)
	must(t, err)
	eq(t, got, "")
	nodes = []any{Object{"createdAt": "2026-11-04T10:00:00Z"}}
	got, err = api.GitHubTitleChanged(3)
	must(t, err)
	eq(t, got, "2026-11-04T10:00:00Z")
	if !strings.HasSuffix(srv.urls[0], "/graphql") {
		t.Fatalf("url: %s", srv.urls[0])
	}
}
