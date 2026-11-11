package ghjira

import (
	"strings"
	"testing"
	"time"
)

const (
	alice = "acct-alice"
	bob   = "acct-bob"
)

// assigneeSyncer syncs one issue with alice and bob mapped; carol is unmapped.
func assigneeSyncer(t *testing.T) *syncer {
	t.Helper()
	c := testConfig(t, Object{"assignee_map": Object{"alice": alice, "Bob": bob}})
	x := &syncer{t: t, c: c, api: newFake()}
	x.api.createConfig = c
	return x
}

func assigneeEntry(t *testing.T, x *syncer, entries []*Entry) *Entry {
	t.Helper()
	op := token(x.c, OpAssignee, x.api.source.idString())
	for _, e := range entries {
		if e.Operation == op {
			return e
		}
	}
	return nil
}

func agreedAssignee(t *testing.T, s *State) string {
	t.Helper()
	a := s.Pairs["1"].Assignee
	if a == nil {
		t.Fatal("no agreed assignee recorded")
	}
	return *a
}

func githubLogins(f *fakeAPI) string {
	var logins []string
	for _, u := range f.source.Assignees {
		logins = append(logins, u.Login)
	}
	return strings.Join(logins, ",")
}

// inSync links the issue with alice assigned on both sides and agreed.
func (x *syncer) assigneesInSync() *State {
	x.t.Helper()
	x.api.setGitHubAssignees("alice")
	s := x.linked()
	eq(x.t, x.api.jira.assignee(), alice)
	eq(x.t, agreedAssignee(x.t, s), alice)
	return s
}

func TestAssigneeMapValidation(t *testing.T) {
	t.Parallel()
	base := `{"repository": "acme/test", "jira_url": "https://acme.atlassian.net", "jira_project": "ENG",
		"jira_default_issue_type": "10001", `
	c, err := ParseConfig([]byte(base + `"assignee_map": {"alice": "a1", "bob": "b1", "mary-jane_Acme": "m1"}}`))
	must(t, err)
	account, ok := c.jiraAccount("ALICE")
	eq(t, []any{account, ok}, []any{"a1", true})
	// Enterprise Managed User logins end in _<shortcode>.
	account, ok = c.jiraAccount("Mary-Jane_acme")
	eq(t, []any{account, ok}, []any{"m1", true})
	for name, tail := range map[string]string{
		"not an object":      `"assignee_map": ["alice"]}`,
		"empty":              `"assignee_map": {}}`,
		"empty account":      `"assignee_map": {"alice": ""}}`,
		"bad login":          `"assignee_map": {"al ice": "a1"}}`,
		"leading @":          `"assignee_map": {"@alice": "a1"}}`,
		"login twice":        `"assignee_map": {"alice": "a1", "Alice": "a2"}}`,
		"account twice":      `"assignee_map": {"alice": "a1", "bob": "a1"}}`,
		"fixed assignee too": `"assignee_map": {"alice": "a1"}, "jira_create_fields": {"assignee": {"accountId": "a1"}}}`,
	} {
		if _, err := ParseConfig([]byte(base + tail)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, err = ParseConfig([]byte(base + `"assignee_map": null}`))
	must(t, err)
	eq(t, c.AssigneeMap == nil, true)
}

func TestNewJiraIssueGetsGitHubAssignee(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	x.api.setGitHubAssignees("carol", "Alice") // carol is unmapped; logins ignore case
	s := x.linked()
	eq(t, x.api.jira.assignee(), alice)
	eq(t, agreedAssignee(t, s), alice)
	n := len(x.api.writes)
	_, entries := x.ok(s)
	if e := assigneeEntry(t, x, entries); e != nil {
		t.Fatalf("planned %s after creation", e.Kind)
	}
	eq(t, len(x.api.writes), n)
}

// A project's default assignee, given when the issue is created from GitHub without
// one, is not a choice: Jira is unassigned to match GitHub.
func TestNewJiraIssueDropsDefaultAssignee(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	x.api.defaultAssignee = bob
	s := x.linked()
	eq(t, s.Pairs["1"].Assignee == nil, true)
	s, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Destination, e.Assignee}, []any{KindSyncAssignee, SideJira, "nobody"})
	if !strings.Contains(e.Reason, "has not changed since the issue was created from GitHub") {
		t.Fatalf("reason: %s", e.Reason)
	}
	eq(t, x.api.jira.assignee(), "")
	eq(t, githubLogins(x.api), "")
	eq(t, agreedAssignee(t, s), "")
}

// Someone assigning the Jira issue afterwards is a choice, so GitHub is filled.
func TestFirstAssigneeFillsUnassignedGitHub(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.linked()
	x.api.setJiraAssignee(bob)
	x.api.assigneeChanged = "2026-11-10T12:00:00.000+0000"
	pair := s.Pairs["1"]
	pair.Assignee = nil
	s.Pairs["1"] = pair
	s, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Destination, e.Assignee}, []any{KindSyncAssignee, SideGitHub, "@Bob"})
	eq(t, githubLogins(x.api), "Bob")
	eq(t, agreedAssignee(t, s), bob)
}

func TestFirstAssigneeFillsUnassignedJira(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.linked()
	x.api.setGitHubAssignees("alice")
	s, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Destination, e.Assignee}, []any{KindSyncAssignee, SideJira, "@alice"})
	eq(t, x.api.jira.assignee(), alice)
	eq(t, agreedAssignee(t, s), alice)
}

func TestFirstAssigneeConflictBlocks(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.linked()
	x.api.setGitHubAssignees("alice")
	x.api.setJiraAssignee(bob)
	_, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, e.Kind, KindBlocked)
	if !strings.Contains(e.Reason, "no agreed assignee recorded; align them manually") {
		t.Fatalf("reason: %s", e.Reason)
	}
}

func TestGitHubAssigneeChangeMovesJira(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.assigneesInSync()
	x.api.setGitHubAssignees("carol", "bob")
	s, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, []any{e.Destination, e.Assignee}, []any{SideJira, "@Bob"})
	eq(t, x.api.jira.assignee(), bob)
	eq(t, agreedAssignee(t, s), bob)

	x.api.setGitHubAssignees("carol")
	s, _ = x.ok(s)
	eq(t, x.api.jira.assignee(), "")
	eq(t, agreedAssignee(t, s), "")
}

// Jira's assignee replaces GitHub's mapped assignees; unmapped ones stay.
func TestJiraAssigneeChangeMovesGitHub(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	x.api.setGitHubAssignees("alice", "carol")
	s := x.linked()
	x.api.setJiraAssignee(bob)
	s, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, []any{e.Destination, e.Assignee}, []any{SideGitHub, "@Bob"})
	eq(t, githubLogins(x.api), "carol,Bob")
	eq(t, agreedAssignee(t, s), bob)

	x.api.setJiraAssignee("")
	s, _ = x.ok(s)
	eq(t, githubLogins(x.api), "carol")
	eq(t, agreedAssignee(t, s), "")
}

// GitHub can have several assignees; it agrees with Jira while Jira's is among them.
func TestSeveralGitHubAssigneesHoldJiras(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.assigneesInSync()
	x.api.setGitHubAssignees("bob", "alice")
	n := len(x.api.writes)
	_, entries := x.ok(s)
	if e := assigneeEntry(t, x, entries); e != nil {
		t.Fatalf("planned %s", e.Kind)
	}
	eq(t, len(x.api.writes), n)
}

func TestBothAssigneesChangedBlocks(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.assigneesInSync()
	x.api.setGitHubAssignees("bob")
	x.api.setJiraAssignee("")
	_, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, e.Kind, KindBlocked)
	if !strings.Contains(e.Reason, "Both sides changed since the agreed assignee @alice") {
		t.Fatalf("reason: %s", e.Reason)
	}
}

func TestUnmappedJiraAssigneeIsExcluded(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.assigneesInSync()
	x.api.setJiraAssignee("acct-dave")
	_, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Reason}, []any{KindExcluded, "Jira assignee is not in assignee_map; assignee sync skipped"})
	eq(t, githubLogins(x.api), "alice")
}

// GitHub drops a login it cannot assign instead of refusing it; that is reported as
// rejected, not recorded as agreed.
func TestUnassignableGitHubLoginIsRejected(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.assigneesInSync()
	x.api.unassignable = map[string]bool{"Bob": true}
	x.api.setJiraAssignee(bob)
	s, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, e.Outcome, OutcomeRejected)
	if !strings.Contains(e.Reason, "GitHub did not assign @Bob") {
		t.Fatalf("reason: %s", e.Reason)
	}
	eq(t, agreedAssignee(t, s), alice)
}

func TestRefusedJiraAssigneeIsRejected(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.assigneesInSync()
	x.api.setGitHubAssignees("bob")
	x.api.refuse = []int{400}
	s, entries := x.ok(s)
	e := assigneeEntry(t, x, entries)
	eq(t, e.Outcome, OutcomeRejected)
	eq(t, x.api.jira.assignee(), alice)
	eq(t, agreedAssignee(t, s), alice)
}

func TestAssigneeAlreadyMatchedWhenApplied(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.assigneesInSync()
	x.api.setGitHubAssignees("bob")
	entries, err := Plan(x.c, s, x.api, time.Now())
	must(t, err)
	x.api.setJiraAssignee(bob) // someone made the same change before apply
	s, err = Apply(x.c, s, x.api, entries, x.save)
	must(t, err)
	eq(t, assigneeEntry(t, x, entries).Outcome, OutcomeAlreadyDone)
	eq(t, agreedAssignee(t, s), bob)
}

func TestAssigneeChangedAfterPlanRetries(t *testing.T) {
	t.Parallel()
	x := assigneeSyncer(t)
	s := x.assigneesInSync()
	x.api.setGitHubAssignees("bob")
	entries, err := Plan(x.c, s, x.api, time.Now())
	must(t, err)
	x.api.setGitHubAssignees("carol")
	_, err = Apply(x.c, s, x.api, entries, x.save)
	wantRetry(t, entries, err, "Assignee changed after plan")
}

func TestAssigneeSyncOffLeavesAssigneesAlone(t *testing.T) {
	t.Parallel()
	x := newSyncer(t)
	x.api.setGitHubAssignees("alice")
	s := x.linked()
	eq(t, x.api.jira.assignee(), "")
	x.api.setJiraAssignee(bob)
	s, _ = x.ok(s)
	eq(t, githubLogins(x.api), "alice")
	eq(t, s.Pairs["1"].Assignee == nil, true)
}

func TestAssigneeAPIRequests(t *testing.T) {
	t.Parallel()
	srv := newServer(t, ok(Object{"id": "200", "key": "ENG-1"}))
	api := srv.client(t)
	api.c = testConfig(t, Object{"assignee_map": Object{"alice": alice}})
	source := newFake().source
	source.Assignees = []gitHubUser{{Login: "carol"}, {Login: "alice"}}
	_, err := api.CreateIssue(&source, "")
	must(t, err)
	eq(t, string(canonical(asObject(srv.bodies[0]["fields"])["assignee"], false)), `{"accountId":"acct-alice"}`)

	must(t, api.SetJiraAssignee("200", alice))
	must(t, api.SetJiraAssignee("200", ""))
	must(t, api.SetGitHubAssignees(1, []string{"carol", "alice"}))
	eq(t, srv.urls[1:], []string{"/rest/api/3/issue/200/assignee", "/rest/api/3/issue/200/assignee", "/repos/acme/test/issues/1"})
	eq(t, string(canonical(srv.bodies[1], false)), `{"accountId":"acct-alice"}`)
	eq(t, string(canonical(srv.bodies[2], false)), `{"accountId":null}`)
	eq(t, string(canonical(srv.bodies[3], false)), `{"assignees":["carol","alice"]}`)
	// Unassigning everyone sends an empty list: GitHub refuses null with 422.
	must(t, api.SetGitHubAssignees(1, nil))
	eq(t, string(canonical(srv.bodies[4], false)), `{"assignees":[]}`)

	// Without assignee_map a new issue gets no assignee field.
	api.c = testConfig(t, nil)
	_, err = api.CreateIssue(&source, "")
	must(t, err)
	if _, set := asObject(srv.bodies[5]["fields"])["assignee"]; set {
		t.Fatal("assignee set with assignee sync off")
	}
}
