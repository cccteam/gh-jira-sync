package ghjira

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// reverseWorld has labeled Jira issues and creates GitHub issues for them.
type reverseWorld struct {
	*fakeAPI
	selected   []*JiraIssue
	github     map[int]*GitHubIssue
	nextNumber int
	loseCreate bool
	refuseType bool
	searches   int
	finds      []string
}

func newReverseWorld(t *testing.T) (*syncer, *reverseWorld) {
	x := newSyncer(t)
	label := "sync-github"
	x.c.JiraSelectionLabel = &label
	x.c.JiraIssueTypes = map[string]string{"Feature": "10387", "Task": "10001"}
	w := &reverseWorld{fakeAPI: x.api, github: map[int]*GitHubIssue{}, nextNumber: 20}
	return x, w
}

func (w *reverseWorld) addJira(id int, summary, description, typeID string) *JiraIssue {
	j := &JiraIssue{ID: strconv.Itoa(id), Key: "ENG-" + strconv.Itoa(id)}
	j.Fields.Project.Key = "ENG"
	j.Fields.Summary = summary
	if description != "" {
		j.Fields.Description = toADF(description)
	}
	j.Fields.IssueType.ID = typeID
	w.selected = append(w.selected, j)
	return j
}

func (w *reverseWorld) run(x *syncer, s *State) (*State, []*Entry, error) {
	if s == nil {
		s = NewState(x.c)
	}
	entries, err := Plan(x.c, s, w, time.Now())
	if err != nil {
		return nil, entries, err
	}
	result, err := Apply(x.c, s, w, entries, x.save)
	return result, entries, err
}

func (w *reverseWorld) ok(t *testing.T, x *syncer, s *State) (*State, []*Entry) {
	t.Helper()
	s, entries, err := w.run(x, s)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return s, entries
}

func (w *reverseWorld) Issues() ([]GitHubIssue, error) {
	var out []GitHubIssue
	for _, issue := range w.github {
		out = append(out, *issue)
	}
	return out, nil
}
func (w *reverseWorld) Issue(n int) (*GitHubIssue, error) {
	if issue, ok := w.github[n]; ok {
		c := *issue
		return &c, nil
	}
	return nil, nil
}
func (w *reverseWorld) JiraIssue(id string) (*JiraIssue, error) {
	for _, j := range w.selected {
		if j.ID == id {
			c := *j
			return &c, nil
		}
	}
	return nil, nil
}
func (w *reverseWorld) SelectedJiraIssues() ([]JiraIssue, error) {
	w.searches++
	var out []JiraIssue
	for _, j := range w.selected {
		out = append(out, *j)
	}
	return out, nil
}
func (w *reverseWorld) CreateGitHubIssue(title, body string, labels []string, issueType string) (*GitHubIssue, error) {
	if err := w.maybeRefuse(); err != nil {
		return nil, err
	}
	if issueType != "" && w.refuseType {
		return nil, &HTTPStatusError{Status: 422, Msg: "unknown type"}
	}
	w.nextNumber++
	issue := &GitHubIssue{ID: int64(500 + w.nextNumber), Number: w.nextNumber, Title: title, Body: body, State: "open"}
	if issueType != "" {
		issue.Type = &struct {
			Name string `json:"name"`
		}{issueType}
	}
	w.github[issue.Number] = issue
	w.writes = append(w.writes, write{"github issue", title})
	if w.loseCreate {
		return nil, syncErr("lost response")
	}
	return issue, nil
}
func (w *reverseWorld) FindGitHubIssue(op, started string) (*GitHubIssue, error) {
	w.finds = append(w.finds, started)
	for _, issue := range w.github {
		if hasMarker(Comment{Side: SideGitHub, Body: issue.Body}, op) {
			c := *issue
			return &c, nil
		}
	}
	return nil, nil
}

func reverseEntry(entries []*Entry, key string) *Entry {
	for _, e := range entries {
		if e.JiraKey == key {
			return e
		}
	}
	return nil
}

func TestJiraIssueCreatesGitHubIssueOnce(t *testing.T) {
	x, w := newReverseWorld(t)
	w.addJira(5, "Login fails on Safari", "Steps:\n- open\n- click", "10387")
	s, entries := w.ok(t, x, nil)
	e := reverseEntry(entries, "ENG-5")
	eq(t, []any{e.Kind, e.GitHub}, []any{KindCreateGitHubIssue, 21})

	issue := w.github[21]
	op := token(x.c, OpJiraIssue, "5")
	eq(t, issue.Title, "Login fails on Safari")
	eq(t, issue.Body, "From Jira: https://acme.atlassian.net/browse/ENG-5\n\nSteps:\n- open\n- click\n\n<!-- gh-jira-sync:"+op+" -->")
	eq(t, issue.typeName(), "Feature") // Jira 10387 maps back to GitHub's Feature
	eq(t, s.Pairs["21"], Pair{GitHub: 21, GitHubID: "521", JiraID: "5", JiraKey: "ENG-5"})

	// Linked now: neither direction creates anything more.
	writes := len(w.writes)
	_, entries = w.ok(t, x, s)
	eq(t, len(w.writes), writes)
	if reverseEntry(entries, "ENG-5") != nil {
		t.Fatal("linked Jira issue planned again")
	}
}

func TestLostGitHubCreationIsRecoveredByMarker(t *testing.T) {
	x, w := newReverseWorld(t)
	w.addJira(5, "Title", "", "")
	w.loseCreate = true
	if _, _, err := w.run(x, nil); err == nil {
		t.Fatal("expected the lost response to fail the run")
	}
	pending := x.last()
	started := pending.Operations[token(x.c, OpJiraIssue, "5")].Started
	w.loseCreate = false
	s, entries := w.ok(t, x, pending)
	eq(t, reverseEntry(entries, "ENG-5").Kind, KindLinkGitHubIssue)
	eq(t, w.finds, []string{started})
	eq(t, len(w.github), 1) // never created twice
	eq(t, s.Pairs["21"].JiraKey, "ENG-5")
}

func TestPendingGitHubCreationWithoutMarkerBlocks(t *testing.T) {
	x, w := newReverseWorld(t)
	w.addJira(5, "Title", "", "")
	w.loseCreate = true
	w.run(x, nil)
	delete(w.github, 21) // not visible yet
	w.loseCreate = false
	_, entries := w.ok(t, x, x.last())
	eq(t, reverseEntry(entries, "ENG-5").Kind, KindBlocked)
	eq(t, len(w.github), 0)
}

func TestRefusedGitHubCreationRetriesAndTypeFallback(t *testing.T) {
	x, w := newReverseWorld(t)
	w.addJira(5, "Title", "", "10387")
	w.refuse = []int{400}
	s, entries := w.ok(t, x, nil)
	eq(t, reverseEntry(entries, "ENG-5").Outcome, OutcomeRejected)
	eq(t, len(w.github), 0)

	w.refuseType = true // the Feature type does not exist in this organization
	s, entries = w.ok(t, x, s)
	e := reverseEntry(entries, "ENG-5")
	eq(t, []any{e.Kind, e.PreviousRejection, e.Outcome}, []any{KindCreateGitHubIssue, 400, Outcome("")})
	if !strings.Contains(e.Reason, "created without a type") {
		t.Fatalf("reason: %s", e.Reason)
	}
	eq(t, w.github[21].typeName(), "")
	eq(t, s.Pairs["21"].JiraKey, "ENG-5")
}

func TestUncopyableJiraIssuesBlock(t *testing.T) {
	x, w := newReverseWorld(t)
	media := w.addJira(5, "Has media", "", "")
	media.Fields.Description = Object{"type": "doc", "version": 1, "content": []any{Object{"type": "mediaSingle"}}}
	w.addJira(6, "  ", "", "")
	_, entries := w.ok(t, x, nil)
	eq(t, reverseEntry(entries, "ENG-5").Kind, KindBlocked)
	eq(t, reverseEntry(entries, "ENG-6").Reason, "Jira summary is empty")
	eq(t, len(w.github), 0)
}

func TestJiraIssueChangedAfterPlanAborts(t *testing.T) {
	x, w := newReverseWorld(t)
	j := w.addJira(5, "Before", "", "")
	entries, err := Plan(x.c, NewState(x.c), w, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	j.Fields.Summary = "After"
	if _, err := Apply(x.c, NewState(x.c), w, entries, x.save); err == nil {
		t.Fatal("created from a Jira issue that changed after plan")
	}
	eq(t, len(w.github), 0)
}

func TestReverseCreationOffAndBaselines(t *testing.T) {
	x, w := newReverseWorld(t)
	x.c.JiraSelectionLabel = nil
	w.addJira(5, "Title", "", "")
	w.ok(t, x, nil)
	eq(t, w.searches, 0)

	y, v := newReverseWorld(t)
	enableStatus(y)
	y.c.SyncParents = true
	v.addJira(5, "Title", "", "")
	s, _ := v.ok(t, y, nil)
	eq(t, []string{agreed(t, &State{Pairs: map[string]Pair{"1": s.Pairs["21"]}}), agreedParent(s, 21)}, []string{"open", "none"})

	for _, bad := range []string{`""`, `"two words"`, `"q\"uote"`, `5`} {
		if _, err := ParseConfig([]byte(`{"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
			"jira_project": "ENG", "jira_issue_type": "10001", "jira_selection_label": ` + bad + `}`)); err == nil {
			t.Errorf("jira_selection_label %s accepted", bad)
		}
	}
	eq(t, *testConfig(t, Object{"jira_selection_label": "sync-github"}).JiraSelectionLabel, "sync-github")
}

func TestReverseAPIRequests(t *testing.T) {
	var jql []string
	op := strings.Repeat("f", 64)
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql"):
			jql = append(jql, r.URL.Query().Get("jql"))
			if r.URL.Query().Get("nextPageToken") == "" {
				return 200, nil, Object{"issues": []any{Object{"id": "5", "key": "ENG-5"}}, "nextPageToken": "p2"}
			}
			return 200, nil, Object{"issues": []any{Object{"id": "6", "key": "ENG-6"}}}
		case r.Method == "POST":
			return 201, nil, Object{"id": 777, "number": 21}
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues"):
			if r.URL.Query().Get("since") == "" || r.URL.Query().Get("state") != "all" {
				return 400, nil, nil
			}
			return 200, nil, []any{
				Object{"id": 1, "number": 20, "body": "> <!-- gh-jira-sync:" + op + " -->\n\nquoted"},
				Object{"id": 2, "number": 21, "body": "From Jira\n\n<!-- gh-jira-sync:" + op + " -->"},
				Object{"id": 3, "number": 22, "body": "<!-- gh-jira-sync:" + op + " -->", "pull_request": Object{}}}
		}
		return 200, nil, nil
	})
	api := srv.client(t)
	label := "sync-github"
	api.c.JiraSelectionLabel = &label
	for state, filter := range map[string]string{"open": "statusCategory != Done", "closed": "statusCategory = Done", "all": ""} {
		jql = nil
		api.c.IssueState = state
		issues, err := api.SelectedJiraIssues()
		if err != nil || len(issues) != 2 {
			t.Fatalf("%s: %v %v", state, issues, err)
		}
		if !strings.Contains(jql[0], `labels = "sync-github"`) || !strings.Contains(jql[0], filter) ||
			(filter == "" && strings.Contains(jql[0], "statusCategory")) {
			t.Errorf("%s jql: %s", state, jql[0])
		}
	}
	created, err := api.CreateGitHubIssue("T", "B", []string{"sync-jira"}, "Feature")
	if err != nil || created.Number != 21 {
		t.Fatalf("create: %+v %v", created, err)
	}
	eq(t, string(canonical(srv.bodies[len(srv.bodies)-1], false)), `{"body":"B","labels":["sync-jira"],"title":"T","type":"Feature"}`)
	found, err := api.FindGitHubIssue(op, time.Now().UTC().Format(time.RFC3339))
	if err != nil || found == nil || found.Number != 21 {
		t.Fatalf("find: %+v %v", found, err)
	}
	if _, err := api.FindGitHubIssue(op, ""); err == nil {
		t.Fatal("pending creation without start time accepted")
	}
}

func TestGitHubIssueFromJiraIsNeverSentBackToJira(t *testing.T) {
	x, w := newReverseWorld(t)
	w.addJira(5, "Title", "", "")
	w.loseCreate = true
	w.run(x, nil)
	w.loseCreate = false
	// #21 now exists in GitHub with the selection label but is unlinked; GitHub → Jira
	// creation must leave it to recovery instead of making a second Jira issue.
	entries, err := Plan(x.c, x.last(), w, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind == KindCreateIssue {
			t.Fatalf("planned a Jira issue for GitHub #%d, which came from Jira", e.GitHub)
		}
	}
	// With its journal lost, such an issue is blocked for review rather than sent back.
	_, entries = w.ok(t, x, nil)
	found := false
	for _, e := range entries {
		found = found || (e.GitHub == 21 && e.Kind == KindBlocked && strings.Contains(e.Reason, "created from Jira"))
		if e.Kind == KindCreateIssue {
			t.Fatal("sent a Jira-created issue back to Jira")
		}
	}
	if !found {
		t.Fatal("unjournaled Jira-created issue was not blocked")
	}
}

func TestJiraIssueFromGitHubIsNeverSentBackToGitHub(t *testing.T) {
	x, w := newReverseWorld(t)
	j := w.addJira(5, "Came from GitHub", "", "")
	op := token(x.c, OpIssue, "42")
	j.Properties.Identity = &IssueIdentity{Operation: op, GitHubID: "42"}

	s := NewState(x.c) // its GitHub → Jira creation is still pending recovery
	s.Operations[op] = Operation{Kind: OpIssue, SourceID: "42", GitHub: 1, Status: StatusPending}
	entries, err := Plan(x.c, s, w, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if e := reverseEntry(entries, "ENG-5"); e != nil {
		t.Fatalf("planned %s for a Jira issue created from GitHub", e.Kind)
	}
	// With no journal, it is blocked rather than sent back.
	_, entries = w.ok(t, x, nil)
	eq(t, reverseEntry(entries, "ENG-5").Kind, KindBlocked)
	eq(t, len(w.github), 0)
}
