package ghjira

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const epicType = "10000"

// ownEpics makes the linked Jira issue an Epic and Epics Jira-owned.
func ownEpics(x *syncer) {
	x.c.JiraOwnedIssueTypes = []string{epicType}
	x.api.jira.Fields.IssueType.ID, x.api.jira.Fields.IssueType.Name = epicType, "Epic"
}

func TestOwnershipConfig(t *testing.T) {
	t.Parallel()
	c := testConfig(t, Object{
		"sync_parents": true, "jira_owned_issue_types": []any{"Epic", "10001"},
		"ignored_jira_parents": []any{"ITP-2019"},
	})
	eq(t, []any{c.JiraOwnedIssueTypes, c.IgnoredJiraParents}, []any{[]string{"Epic", "10001"}, []string{"ITP-2019"}})
	c = testConfig(t, nil)
	eq(t, []any{len(c.JiraOwnedIssueTypes), len(c.IgnoredJiraParents)}, []any{0, 0})

	for name, tc := range map[string]struct {
		extra Object
		want  string
	}{
		"owned not a list":        {Object{"sync_parents": true, "jira_owned_issue_types": "Epic"}, "jira_owned_issue_types must"},
		"owned duplicate by case": {Object{"sync_parents": true, "jira_owned_issue_types": []any{"Epic", "EPIC"}}, "jira_owned_issue_types must"},
		"owned empty name":        {Object{"sync_parents": true, "jira_owned_issue_types": []any{" "}}, "jira_owned_issue_types must"},
		"owned with nothing to own": {
			Object{"jira_owned_issue_types": []any{"Epic"}}, "needs status_sync, project_status, or sync_parents",
		},
		"ignored not a key":         {Object{"sync_parents": true, "ignored_jira_parents": []any{"itp-2019"}}, "ignored_jira_parents must"},
		"ignored duplicate":         {Object{"sync_parents": true, "ignored_jira_parents": []any{"A-1", "A-1"}}, "ignored_jira_parents must"},
		"ignored without parents":   {Object{"ignored_jira_parents": []any{"ITP-2019"}}, "ignored_jira_parents needs sync_parents"},
		"ignored number not string": {Object{"sync_parents": true, "ignored_jira_parents": []any{12}}, "ignored_jira_parents must"},
	} {
		raw := Object{
			"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
			"jira_project": "ENG", "jira_default_issue_type": "10001",
		}
		for k, v := range tc.extra {
			raw[k] = v
		}
		if _, err := ParseConfig(canonical(raw, false)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}

func TestCheckProjectResolvesOwnedIssueTypes(t *testing.T) {
	t.Parallel()
	list := []any{
		Object{"id": "10001", "name": "Task", "subtask": false},
		Object{"id": epicType, "name": "Epic", "subtask": false},
		Object{"id": "10302", "name": "Sub-task", "subtask": true},
	}
	project := func() *server {
		return newServer(t, func(*http.Request) (int, http.Header, any) {
			return 200, nil, Object{"projectTypeKey": "software", "issueTypes": list}
		})
	}
	api := project().client(t)
	api.c.JiraOwnedIssueTypes = []string{"epic", "Sub-task", "10001"}
	if err := api.CheckProject(); err != nil {
		t.Fatalf("valid owned types rejected: %v", err)
	}
	eq(t, api.c.JiraOwnedIssueTypes, []string{epicType, "10302", "10001"})

	api = project().client(t)
	api.c.JiraOwnedIssueTypes = []string{"Initiative"}
	if err := api.CheckProject(); err == nil || !strings.Contains(err.Error(), "Initiative") {
		t.Errorf("unknown owned type: got %v", err)
	}
}

func TestOwnedProjectMoveIsSetBackToJira(t *testing.T) {
	t.Parallel()
	x := projectSyncer(t)
	s := projectInSync(t, x)
	ownEpics(x)
	x.setOption("In Progress")
	s, entries := x.ok(s)
	e := projectEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Destination, e.Status}, []any{KindSyncProjectStatus, SideProject, "Todo"})
	if !strings.Contains(e.Reason, "Epic issues are owned by Jira") {
		t.Errorf("reason %q does not explain ownership", e.Reason)
	}
	eq(t, lastWrite(x.api), write{"project", "Todo"})
	eq(t, x.api.jira.Fields.Status.Name, "To Do")
	eq(t, agreedOption(s), "Todo")

	// Jira still moves the project as usual.
	x.api.jira.Fields.Status = jiraStatus("In Progress", "indeterminate")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"project", "In Progress"})
	eq(t, agreedOption(s), "In Progress")
}

func TestOwnedUnmappedProjectOptionIsSetBackInsteadOfBlocking(t *testing.T) {
	t.Parallel()
	x := projectSyncer(t)
	s := projectInSync(t, x)
	ownEpics(x)
	x.setOption("Blocked") // not in the map
	_, entries := x.ok(s)
	e := projectEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Destination, e.Status}, []any{KindSyncProjectStatus, SideProject, "Todo"})
	eq(t, lastWrite(x.api), write{"project", "Todo"})
}

func TestOwnedProjectConflictFollowsJiraEvenWhenProjectIsLater(t *testing.T) {
	t.Parallel()
	x := projectSyncer(t)
	s := projectInSync(t, x)
	ownEpics(x)
	x.api.jira.Fields.Status = jiraStatus("In Review", "indeterminate")
	x.api.jiraChanged = "2026-01-01T00:00:00Z"
	x.setOption("Done") // later than Jira's change, which would normally move Jira
	_, entries := x.ok(s)
	e := projectEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Destination, e.Status}, []any{KindSyncProjectStatus, SideProject, "In Progress"})
	eq(t, x.api.jira.Fields.Status.Name, "In Review")
}

func TestOwnedGitHubCloseIsReopenedInProjectMode(t *testing.T) {
	t.Parallel()
	x := projectSyncer(t)
	s := projectInSync(t, x)
	ownEpics(x)
	x.api.source.State = "closed"
	x.setOption("Done") // the project's "Item closed" workflow
	s, entries := x.ok(s)
	st := statusEntries(entries)
	eq(t, len(st), 1)
	eq(t, []any{st[0].Kind, st[0].Destination, st[0].Status}, []any{KindSyncStatus, SideGitHub, "open"})
	eq(t, x.api.source.State, "open")
	eq(t, x.api.jira.Fields.Status.Name, "To Do")
	for _, w := range x.api.writes {
		if w.kind == "transition" {
			t.Fatalf("Jira-owned issue was transitioned: %v", x.api.writes)
		}
	}
	eq(t, []string{agreed(t, s), agreedOption(s)}, []string{"open", "Todo"})
}

func TestOwnedGitHubCloseIsReopenedWithStatusSync(t *testing.T) {
	t.Parallel()
	x := newSyncer(t)
	enableStatus(x)
	s := x.linked()
	ownEpics(x)
	x.api.source.State = "closed"
	s, entries := x.ok(s)
	st := statusEntries(entries)
	eq(t, len(st), 1)
	eq(t, []any{st[0].Kind, st[0].Destination, st[0].Status}, []any{KindSyncStatus, SideGitHub, "open"})
	eq(t, lastWrite(x.api), write{"state", "open"})
	eq(t, x.api.jira.done(), false)
	eq(t, agreed(t, s), "open")

	// Jira closing still closes GitHub.
	x.api.jira.Fields.Status = jiraStatus("Done", "done")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"state", "closed"})
	eq(t, agreed(t, s), "closed")
}

func TestOwnedDisagreementWithoutAgreementFollowsJira(t *testing.T) {
	t.Parallel()
	x := newSyncer(t)
	s := x.linked() // no status mode yet, so nothing is agreed
	enableStatus(x)
	ownEpics(x)
	x.api.source.State = "closed"
	_, entries := x.ok(s)
	st := statusEntries(entries)
	eq(t, len(st), 1)
	eq(t, []any{st[0].Kind, st[0].Destination}, []any{KindSyncStatus, SideGitHub})
	eq(t, x.api.source.State, "open")
}

func TestOwnedGitHubParentChangeIsSetBack(t *testing.T) {
	t.Parallel()
	x, w, s := newParentWorld(t)
	x.c.JiraOwnedIssueTypes = []string{epicType}
	w.jira["302"].Fields.IssueType.ID = epicType
	w.ghParent[2] = 1
	w.setJiraParent(2, "ENG-1")
	s, _ = w.ok(t, x, s)
	eq(t, agreedParent(s, 2), "#1")

	w.ghParent[2] = 3 // moved in GitHub
	s, entries := w.ok(t, x, s)
	e := parentEntry(entries, 2)
	eq(t, []any{e.Kind, e.Destination, e.Parent}, []any{KindSyncParent, SideGitHub, "#1"})
	eq(t, lastWrite(w.fakeAPI), write{"github parent", "#1"})
	eq(t, w.jira["302"].Fields.Parent.Key, "ENG-1")
	eq(t, agreedParent(s, 2), "#1")

	// With nothing agreed, Jira still wins instead of blocking.
	x2, w2, s2 := newParentWorld(t)
	x2.c.JiraOwnedIssueTypes = []string{epicType}
	w2.jira["302"].Fields.IssueType.ID = epicType
	w2.ghParent[2] = 1
	_, entries = w2.ok(t, x2, s2)
	e = parentEntry(entries, 2)
	eq(t, []any{e.Kind, e.Destination, e.Parent}, []any{KindSyncParent, SideGitHub, "none"})
}

func TestIgnoredJiraParentSkipsParentSync(t *testing.T) {
	t.Parallel()
	x, w, s := newParentWorld(t)
	x.c.IgnoredJiraParents = []string{"ITP-2019"}
	w.setJiraParent(1, "ITP-2019")
	w.ghParent[2] = 1
	w.setJiraParent(2, "ENG-1")
	writes := len(w.writes)
	_, entries := w.ok(t, x, s)
	var e *Entry
	for _, entry := range entries {
		if entry.GitHub == 1 && entry.Operation == token(x.c, OpParent, "101") {
			e = entry
		}
	}
	if e == nil {
		t.Fatal("no parent entry for the issue under the ignored parent")
	}
	eq(t, e.Kind, KindExcluded)
	if !strings.Contains(e.Reason, "ITP-2019") {
		t.Errorf("reason %q does not name the ignored parent", e.Reason)
	}
	eq(t, parentEntry(entries, 2).Kind, KindParentAgreed) // other issues still sync
	eq(t, len(w.writes), writes)

	// Without the setting, the same parent blocks for review.
	x2, w2, s2 := newParentWorld(t)
	w2.setJiraParent(1, "ITP-2019")
	_, entries = w2.ok(t, x2, s2)
	for _, entry := range entries {
		if entry.GitHub == 1 && entry.Kind == KindBlocked && strings.Contains(entry.Reason, "ITP-2019") {
			return
		}
	}
	t.Fatal("unignored unlinked parent did not block")
}

func TestOwnedIssueTypeIsNotCreatedInJira(t *testing.T) {
	t.Parallel()
	x := newSyncer(t)
	x.c.JiraIssueTypes = map[string]string{"Epic": epicType}
	x.c.JiraOwnedIssueTypes = []string{epicType}
	x.api.source.Type = typed("Epic").Type
	_, entries := x.ok(nil)
	eq(t, entries[0].Kind, KindExcluded)
	if !strings.Contains(entries[0].Reason, "create it in Jira") {
		t.Errorf("reason %q does not say where to create it", entries[0].Reason)
	}
	eq(t, len(x.api.writes), 0)

	// Other types are still created.
	x.api.source.Type = typed("Bug").Type
	entries, err := Plan(x.c, NewState(x.c), x.api, time.Now())
	must(t, err)
	eq(t, entries[0].Kind, KindCreateIssue)
}
