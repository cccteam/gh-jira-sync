package ghjira

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parentWorld is a fake with several linked issues: #1 and #3 are Epics, #2 a story.
type parentWorld struct {
	*fakeAPI
	issues   map[int]*GitHubIssue
	jira     map[string]*JiraIssue // by Jira ID
	ghParent map[int]int           // child number → parent number
}

func newParentWorld(t *testing.T) (*syncer, *parentWorld, *State) {
	x := newSyncer(t)
	x.c.SyncParents = true
	w := &parentWorld{fakeAPI: x.api, issues: map[int]*GitHubIssue{}, jira: map[string]*JiraIssue{}, ghParent: map[int]int{}}
	s := NewState(x.c)
	for n := 1; n <= 3; n++ {
		w.issues[n] = &GitHubIssue{
			ID: int64(100 + n), Number: n, Title: "issue", State: "open",
			RepositoryURL: "https://api.github.com/repos/acme/test",
		}
		j := &JiraIssue{ID: strconv.Itoa(300 + n), Key: "ENG-" + strconv.Itoa(n)}
		j.Fields.Project.Key = "ENG"
		w.jira[j.ID] = j
		s.Pairs[strconv.Itoa(n)] = Pair{GitHub: n, GitHubID: strconv.Itoa(100 + n), JiraID: j.ID, JiraKey: j.Key}
	}
	return x, w, s
}

// count sets each issue's sub-issue total, as GitHub reports it on the issue.
func (w *parentWorld) count() {
	for n, issue := range w.issues {
		total := 0
		for _, parent := range w.ghParent {
			if parent == n {
				total++
			}
		}
		issue.SubIssuesSummary = &struct {
			Total int `json:"total"`
		}{total}
	}
}

func (w *parentWorld) run(x *syncer, s *State) (*State, []*Entry, error) {
	w.count()
	entries, err := Plan(x.c, s, w, time.Now())
	if err != nil {
		return nil, entries, err
	}
	result, err := Apply(x.c, s, w, entries, x.save)
	return result, entries, err
}

func (w *parentWorld) ok(t *testing.T, x *syncer, s *State) (*State, []*Entry) {
	t.Helper()
	s, entries, err := w.run(x, s)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	return s, entries
}

func (w *parentWorld) Issues() ([]GitHubIssue, error) { return nil, nil }
func (w *parentWorld) Issue(n int) (*GitHubIssue, error) {
	issue := *w.issues[n]
	return &issue, nil
}

func (w *parentWorld) JiraIssue(id string) (*JiraIssue, error) {
	issue := *w.jira[id]
	return &issue, nil
}

func (w *parentWorld) SubIssues(n int) ([]GitHubIssue, error) {
	var children []GitHubIssue
	for child, parent := range w.ghParent {
		if parent == n {
			children = append(children, *w.issues[child])
		}
	}
	return children, nil
}
func (w *parentWorld) GitHubParent(n int) (int, error) { return w.ghParent[n], nil }
func (w *parentWorld) SetGitHubParent(childID int64, parent, _ int) error {
	if err := w.maybeRefuse(); err != nil {
		return err
	}
	child := int(childID - 100)
	if parent == 0 {
		delete(w.ghParent, child)
	} else {
		w.ghParent[child] = parent
	}
	w.writes = append(w.writes, write{"github parent", parentName(parent)})
	return nil
}

func (w *parentWorld) SetJiraParent(id, key string) error {
	if err := w.maybeRefuse(); err != nil {
		return err
	}
	j := w.jira[id]
	if key == "" {
		j.Fields.Parent = nil
	} else {
		j.Fields.Parent = &struct {
			Key string `json:"key"`
		}{key}
	}
	w.writes = append(w.writes, write{"jira parent", key})
	return nil
}

//nolint:unparam // names the child, so each test reads as its scenario
func (w *parentWorld) setJiraParent(child int, key string) {
	j := w.jira[strconv.Itoa(300+child)]
	if key == "" {
		j.Fields.Parent = nil
		return
	}
	j.Fields.Parent = &struct {
		Key string `json:"key"`
	}{key}
}

//nolint:unparam // names the child, so each test reads as its scenario
func parentEntry(entries []*Entry, child int) *Entry {
	for _, e := range entries {
		if e.GitHub == child && (e.Kind == KindSyncParent || e.Kind == KindParentAgreed || strings.Contains(e.Reason, "parent")) {
			return e
		}
	}
	return nil
}

func agreedParent(s *State, child int) string {
	if p := s.Pairs[strconv.Itoa(child)].Parent; p != nil {
		return parentName(*p)
	}
	return "unrecorded"
}

func TestParentsAgreeThenGitHubSubIssueMovesJira(t *testing.T) {
	t.Parallel()
	x, w, s := newParentWorld(t)
	s, entries := w.ok(t, x, s)
	eq(t, parentEntry(entries, 2).Kind, KindParentAgreed)
	eq(t, agreedParent(s, 2), "none")

	w.ghParent[2] = 1 // someone makes #2 a sub-issue of Epic #1
	s, entries = w.ok(t, x, s)
	e := parentEntry(entries, 2)
	eq(t, []any{e.Kind, e.Destination, e.Parent}, []any{KindSyncParent, SideJira, "#1"})
	eq(t, lastWrite(w.fakeAPI), write{"jira parent", "ENG-1"})
	eq(t, agreedParent(s, 2), "#1")

	writes := len(w.writes)
	_, entries = w.ok(t, x, s)
	eq(t, len(w.writes), writes)
	if parentEntry(entries, 2) != nil {
		t.Fatal("in-sync parent produced work")
	}
}

func TestJiraParentChangesMoveGitHub(t *testing.T) {
	t.Parallel()
	x, w, s := newParentWorld(t)
	w.ghParent[2] = 1
	w.setJiraParent(2, "ENG-1")
	s, _ = w.ok(t, x, s)
	eq(t, agreedParent(s, 2), "#1")

	w.setJiraParent(2, "ENG-3") // moved to the other Epic in Jira
	s, _ = w.ok(t, x, s)
	eq(t, lastWrite(w.fakeAPI), write{"github parent", "#3"})
	eq(t, w.ghParent[2], 3)

	w.setJiraParent(2, "") // parent removed in Jira
	s, _ = w.ok(t, x, s)
	eq(t, lastWrite(w.fakeAPI), write{"github parent", "none"})
	if _, still := w.ghParent[2]; still {
		t.Fatal("GitHub sub-issue not removed")
	}
	eq(t, agreedParent(s, 2), "none")
}

func TestParentConflictsBlockForReview(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(w *parentWorld, s *State){
		"not linked to a GitHub issue": func(w *parentWorld, _ *State) { w.setJiraParent(2, "ENG-99") },
		"no agreed parent recorded":    func(w *parentWorld, _ *State) { w.ghParent[2] = 1 },
		"Both sides changed": func(w *parentWorld, s *State) {
			setAgreedParent(s, 2, 0)
			w.ghParent[2] = 1
			w.setJiraParent(2, "ENG-3")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			x, w, s := newParentWorld(t)
			setup(w, s)
			_, entries := w.ok(t, x, s)
			e := parentEntry(entries, 2)
			eq(t, e.Kind, KindBlocked)
			if !strings.Contains(e.Reason, name) {
				t.Fatalf("reason: %s", e.Reason)
			}
			eq(t, len(w.writes), 0)
		})
	}
}

func TestRejectedParentChangeExplainsHierarchy(t *testing.T) {
	t.Parallel()
	x, w, s := newParentWorld(t)
	setAgreedParent(s, 2, 0)
	w.ghParent[2] = 3
	w.refuse = []int{400}
	s, entries := w.ok(t, x, s)
	e := parentEntry(entries, 2)
	eq(t, e.Outcome, OutcomeRejected)
	if !strings.Contains(e.Reason, "Epic above a Story") {
		t.Fatalf("reason: %s", e.Reason)
	}
	eq(t, agreedParent(s, 2), "none")
}

func TestParentAlreadyMatchedBeforeWrite(t *testing.T) {
	t.Parallel()
	x, w, s := newParentWorld(t)
	setAgreedParent(s, 2, 0)
	w.ghParent[2] = 1
	w.count()
	entries, err := Plan(x.c, s, w, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w.setJiraParent(2, "ENG-1") // someone set it in Jira first
	s, err = Apply(x.c, s, w, entries, x.save)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, parentEntry(entries, 2).Outcome, OutcomeAlreadyDone)
	eq(t, len(w.writes), 0)
	eq(t, agreedParent(s, 2), "#1")
}

func TestParentSyncOffAndCreationBaseline(t *testing.T) {
	t.Parallel()
	x, w, s := newParentWorld(t)
	x.c.SyncParents = false
	w.ghParent[2] = 1
	_, entries := w.ok(t, x, s)
	if parentEntry(entries, 2) != nil {
		t.Fatal("parent work while sync_parents is off")
	}
	y := newSyncer(t)
	y.c.SyncParents = true
	eq(t, agreedParent(y.linked(), 1), "none") // new issues start with no parent agreed
	eq(t, testConfig(t, Object{"sync_parents": true}).SyncParents, true)
	if _, err := ParseConfig([]byte(`{"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
		"jira_project": "ENG", "jira_issue_type": "10001", "sync_parents": "yes"}`)); err == nil {
		t.Fatal("non-boolean sync_parents accepted")
	}
}

func TestParentAPIRequests(t *testing.T) {
	t.Parallel()
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/1/sub_issues") && r.Method == "GET":
			return 200, nil, []any{
				Object{"id": 102, "number": 2, "repository_url": "https://api.github.com/repos/acme/test"},
				Object{"id": 900, "number": 9, "repository_url": "https://api.github.com/repos/acme/other"},
			}
		case strings.HasSuffix(r.URL.Path, "/issues/2/parent"):
			return 200, nil, Object{"id": 101, "number": 1, "repository_url": "https://api.github.com/repos/acme/test"}
		case strings.HasSuffix(r.URL.Path, "/issues/3/parent"):
			return 404, nil, nil
		case strings.HasSuffix(r.URL.Path, "/issues/4/parent"):
			return 200, nil, Object{"id": 900, "number": 9, "repository_url": "https://api.github.com/repos/acme/other"}
		}
		return 200, nil, nil
	})
	api := srv.client(t)
	api.c.GitHubURL = "https://api.github.com" // repository_url values use the real API host
	children, err := api.SubIssues(1)
	if err != nil || len(children) != 1 || children[0].Number != 2 {
		t.Fatalf("children: %+v %v", children, err)
	}
	for number, want := range map[int]int{2: 1, 3: 0, 4: 0} {
		if got, err := api.GitHubParent(number); err != nil || got != want {
			t.Errorf("parent of #%d: %d %v, want %d", number, got, err, want)
		}
	}
	last := func() string { return string(canonical(srv.bodies[len(srv.bodies)-1], false)) }
	must(t, api.SetGitHubParent(102, 3, 1))
	eq(t, []string{srv.urls[len(srv.urls)-1], last()}, []string{"/repos/acme/test/issues/3/sub_issues", `{"replace_parent":true,"sub_issue_id":102}`})
	must(t, api.SetGitHubParent(102, 0, 1))
	eq(t, []string{srv.urls[len(srv.urls)-1], last()}, []string{"/repos/acme/test/issues/1/sub_issue", `{"sub_issue_id":102}`})
	must(t, api.SetJiraParent("302", "ENG-1"))
	eq(t, last(), `{"fields":{"parent":{"key":"ENG-1"}}}`)
	must(t, api.SetJiraParent("302", ""))
	eq(t, last(), `{"update":{"parent":[{"set":{"none":true}}]}}`)
	eq(t, srv.urls[len(srv.urls)-1], "/rest/api/3/issue/302")
}
