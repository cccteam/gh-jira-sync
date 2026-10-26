package ghjira

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// subtaskWorld extends parentWorld with GitHub issues still to be created in Jira.
// #1 is an Epic and #2 a Task, both linked; new issues are added with add.
type subtaskWorld struct {
	*parentWorld
	created map[int]string // GitHub number → Jira parent key it was created under
	types   map[int]string // GitHub number → Jira issue type ID it was created with
}

func newSubtaskWorld(t *testing.T) (*syncer, *subtaskWorld, *State) {
	x, pw, s := newParentWorld(t)
	x.c.JiraSubtaskType = "10302"
	pw.jira["301"].Fields.IssueType.HierarchyLevel = 1
	delete(s.Pairs, "3")
	delete(pw.issues, 3)
	return x, &subtaskWorld{parentWorld: pw, created: map[int]string{}, types: map[int]string{}}, s
}

func (w *subtaskWorld) add(n, parent int) {
	w.issues[n] = &GitHubIssue{ID: int64(100 + n), Number: n, Title: "issue " + strconv.Itoa(n), State: "open",
		RepositoryURL: "https://api.github.com/repos/acme/test"}
	if parent != 0 {
		w.ghParent[n] = parent
	}
}

func (w *subtaskWorld) Issues() ([]GitHubIssue, error) {
	var list []GitHubIssue
	for _, n := range slices.Sorted(func(yield func(int) bool) {
		for n := range w.issues {
			if !yield(n) {
				return
			}
		}
	}) {
		list = append(list, *w.issues[n])
	}
	return list, nil
}

func (w *subtaskWorld) CreateIssue(source *GitHubIssue, parentKey string) (*JiraIssue, error) {
	n := source.Number
	j := &JiraIssue{ID: strconv.Itoa(300 + n), Key: "ENG-" + strconv.Itoa(n)}
	j.Fields.Project.Key = "ENG"
	j.Fields.IssueType.ID = "10001"
	if parentKey != "" {
		j.Fields.IssueType.ID, j.Fields.IssueType.HierarchyLevel = "10302", -1
		j.Fields.Parent = &struct {
			Key string `json:"key"`
		}{parentKey}
	}
	w.jira[j.ID] = j
	w.created[n], w.types[n] = parentKey, j.Fields.IssueType.ID
	w.writes = append(w.writes, write{"issue", source.Title})
	issue := *j
	return &issue, nil
}

// Comments are off: each world issue would otherwise share the fake's comment rows.
func subtaskRun(t *testing.T, x *syncer, w *subtaskWorld, s *State) (*State, []*Entry) {
	t.Helper()
	x.c.Comments = false
	w.count()
	entries, err := Plan(x.c, s, w, time.Now())
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	if s, err = Apply(x.c, s, w, entries, x.save); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	return s, entries
}

func entryFor(entries []*Entry, n int, kind Kind) *Entry {
	for _, e := range entries {
		if e.GitHub == n && e.Kind == kind {
			return e
		}
	}
	return nil
}

func TestTaskUnderTaskBecomesJiraSubtask(t *testing.T) {
	x, w, s := newSubtaskWorld(t)
	w.add(4, 2) // a Task under the Task #2
	w.add(5, 1) // a Task under the Epic #1
	s, entries := subtaskRun(t, x, w, s)
	e := entryFor(entries, 4, KindCreateIssue)
	eq(t, []any{e.IssueType, e.Parent}, []any{"10302", "#2"})
	eq(t, []any{w.created[4], w.types[4]}, []any{"ENG-2", "10302"})
	eq(t, agreedParent(s, 4), "#2")
	// Under an Epic, the issue keeps its standard type; parent sync links it next run.
	eq(t, []any{w.created[5], w.types[5], agreedParent(s, 5)}, []any{"", "10001", "none"})

	writes := len(w.writes)
	s, _ = subtaskRun(t, x, w, s)
	eq(t, w.writes[writes:], []write{{"jira parent", "ENG-1"}})
	eq(t, []string{agreedParent(s, 4), agreedParent(s, 5)}, []string{"#2", "#1"})
	writes = len(w.writes)
	subtaskRun(t, x, w, s)
	eq(t, len(w.writes), writes)
}

func TestChildWaitsForParentToReachJira(t *testing.T) {
	x, w, s := newSubtaskWorld(t)
	w.add(6, 7)
	w.add(7, 0) // the parent, a Task, is new too
	s, entries := subtaskRun(t, x, w, s)
	e := entryFor(entries, 6, KindDeferred)
	if e == nil || !strings.Contains(e.Reason, "Waiting for parent #7") {
		t.Fatalf("child not waiting: %+v", entries)
	}
	_, created := w.created[6]
	eq(t, []any{created, w.types[7]}, []any{false, "10001"})

	s, _ = subtaskRun(t, x, w, s)
	eq(t, []any{w.created[6], w.types[6], agreedParent(s, 6)}, []any{"ENG-7", "10302", "#7"})
}

// A parent outside the selection never reaches Jira, so the child does not wait.
func TestUnselectedParentDoesNotHoldChild(t *testing.T) {
	x, w, s := newSubtaskWorld(t)
	w.add(8, 9)
	s, _ = subtaskRun(t, x, w, s)
	eq(t, []any{w.created[8], w.types[8], agreedParent(s, 8)}, []any{"", "10001", "none"})
}

func TestSubtaskModeOffKeepsStandardType(t *testing.T) {
	x, w, s := newSubtaskWorld(t)
	x.c.JiraSubtaskType = ""
	w.add(4, 2)
	s, _ = subtaskRun(t, x, w, s)
	eq(t, []any{w.created[4], w.types[4], agreedParent(s, 4)}, []any{"", "10001", "none"})
}

func TestParentChangedBeforeCreationAborts(t *testing.T) {
	x, w, s := newSubtaskWorld(t)
	x.c.Comments = false
	w.add(4, 2)
	w.count()
	entries, err := Plan(x.c, s, w, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w.ghParent[4] = 1
	if _, err := Apply(x.c, s, w, entries, x.save); err == nil || !strings.Contains(err.Error(), "parent changed") {
		t.Fatalf("expected abort, got %v", err)
	}
	_, created := w.created[4]
	eq(t, created, false)
}

func TestSubtaskConfig(t *testing.T) {
	c := testConfig(t, Object{"sync_parents": true, "jira_subtask_type": "10302"})
	eq(t, c.JiraSubtaskType, "10302")
	for name, extra := range map[string]Object{
		"without sync_parents": {"jira_subtask_type": "10302"},
		"not digits":           {"sync_parents": true, "jira_subtask_type": "Subtask"},
		"a number":             {"sync_parents": true, "jira_subtask_type": 10302},
	} {
		raw := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
			"jira_project": "ENG", "jira_issue_type": "10001"}
		for k, v := range extra {
			raw[k] = v
		}
		if _, err := ParseConfig(canonical(raw, false)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSubtaskAPIRequests(t *testing.T) {
	srv := newServer(t, ok(Object{"id": "200", "key": "ENG-9"}))
	api := srv.client(t)
	api.c.JiraSubtaskType = "10302"
	source := newFake().source
	if _, err := api.CreateIssue(&source, "ENG-2"); err != nil {
		t.Fatal(err)
	}
	fields := asObject(srv.bodies[0]["fields"])
	eq(t, []any{asObject(fields["issuetype"])["id"], asObject(fields["parent"])["key"]}, []any{"10302", "ENG-2"})

	project := func(types ...any) *Client {
		srv := newServer(t, func(*http.Request) (int, http.Header, any) {
			return 200, nil, Object{"projectTypeKey": "software", "issueTypes": types}
		})
		api := srv.client(t)
		api.c.JiraSubtaskType = "10302"
		return api
	}
	task := Object{"id": "10001", "subtask": false}
	if err := project(task, Object{"id": "10302", "subtask": true}).CheckProject(); err != nil {
		t.Fatalf("valid sub-task type rejected: %v", err)
	}
	for name, types := range map[string][]any{
		"standard type": {task, Object{"id": "10302", "subtask": false}},
		"missing":       {task},
	} {
		if err := project(types...).CheckProject(); err == nil || !strings.Contains(err.Error(), "jira_subtask_type") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
