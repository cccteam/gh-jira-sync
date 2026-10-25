package ghjira

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// projectSyncer has a project with Todo / In Progress / Done (plus an unmapped
// Blocked option) and a Jira workflow where any status can move to any other.
func projectSyncer(t *testing.T) *syncer {
	x := newSyncer(t)
	x.c.ProjectStatus = &ProjectStatus{Owner: "acme", OwnerType: "org", ProjectNumber: 1, Field: "Status", AddToProject: true,
		Map: []ProjectStatusMapping{
			{Project: "Todo", Jira: []string{"To Do"}},
			{Project: "In Progress", Jira: []string{"In Progress", "In Review"}},
			{Project: "Done", Jira: []string{"Done"}},
		}}
	x.api.project = &ProjectSnapshot{FieldID: 9, Items: map[int64]ProjectItem{},
		Options: map[string]string{"Todo": "o-todo", "In Progress": "o-prog", "Done": "o-done", "Blocked": "o-blocked"}}
	x.api.transitions = []JiraTransition{
		{ID: "1", Name: "Start", To: jiraStatus("In Progress", "indeterminate")},
		{ID: "2", Name: "Review", To: jiraStatus("In Review", "indeterminate")},
		{ID: "3", Name: "Finish", To: jiraStatus("Done", "done")},
		{ID: "4", Name: "Back", To: jiraStatus("To Do", "new")},
	}
	return x
}

func projectEntry(t *testing.T, x *syncer, entries []*Entry) *Entry {
	t.Helper()
	op := token(x.c, OpProjectStatus, "42")
	for _, e := range entries {
		if e.Operation == op {
			return e
		}
	}
	return nil
}

func agreedOption(s *State) string {
	if p := s.Pairs["1"].ProjectStatus; p != nil {
		return *p
	}
	return "none"
}

func (x *syncer) setOption(name string) {
	item := x.api.project.Items[42]
	item.OptionID = x.api.project.Options[name]
	x.api.project.Items[42] = item
}

// projectInSync links the issue, adds it to the project, and fills its status from Jira.
func projectInSync(t *testing.T, x *syncer) *State {
	t.Helper()
	s := x.linked()
	s, entries := x.ok(s)
	eq(t, projectEntry(t, x, entries).Kind, KindAddToProject)
	eq(t, lastWrite(x.api), write{"add", "project"})
	s, entries = x.ok(s)
	e := projectEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Destination, e.Status}, []any{KindSyncProjectStatus, SideProject, "Todo"})
	eq(t, lastWrite(x.api), write{"project", "Todo"})
	eq(t, agreedOption(s), "Todo")
	writes := len(x.api.writes)
	s, entries = x.ok(s)
	if projectEntry(t, x, entries) != nil || len(x.api.writes) != writes {
		t.Fatal("in-sync pair produced project work")
	}
	return s
}

func TestProjectAddedAndFilledFromJira(t *testing.T) {
	x := projectSyncer(t)
	s := projectInSync(t, x)
	eq(t, agreed(t, s), "open") // creation also records open/closed in project mode
}

func TestProjectStatusMovesJira(t *testing.T) {
	x := projectSyncer(t)
	s := projectInSync(t, x)
	x.setOption("In Progress")
	s, entries := x.ok(s)
	e := projectEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Destination, e.Status}, []any{KindSyncProjectStatus, SideJira, "In Progress"})
	eq(t, lastWrite(x.api), write{"transition", "Start"})
	eq(t, x.api.jira.Fields.Status.Name, "In Progress")
	eq(t, agreedOption(s), "In Progress")

	// Option and Jira status names differ ("Todo" vs "To Do"); the Jira name is the target.
	x.setOption("Todo")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"transition", "Back"})
	eq(t, x.api.jira.Fields.Status.Name, "To Do")
	eq(t, agreedOption(s), "Todo")
}

func TestJiraMovesProjectThroughManyToOneMapping(t *testing.T) {
	x := projectSyncer(t)
	s := projectInSync(t, x)
	x.api.jira.Fields.Status = jiraStatus("In Review", "indeterminate")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"project", "In Progress"})
	eq(t, agreedOption(s), "In Progress")
}

func TestJiraDoneSetsProjectAndClosesGitHub(t *testing.T) {
	x := projectSyncer(t)
	s := projectInSync(t, x)
	x.api.jira.Fields.Status = jiraStatus("Done", "done")
	s, entries := x.ok(s)
	eq(t, kinds(entries)[:2], []Kind{KindSyncStatus, KindSyncProjectStatus})
	eq(t, x.api.writes[len(x.api.writes)-2:], []write{{"state", "closed"}, {"project", "Done"}})
	eq(t, []string{agreed(t, s), agreedOption(s)}, []string{"closed", "Done"})
}

func TestGitHubCloseWaitsForProjectAutomation(t *testing.T) {
	x := projectSyncer(t)
	s := projectInSync(t, x)
	writes := len(x.api.writes)
	x.api.source.State = "closed"
	s, _ = x.ok(s)
	eq(t, len(x.api.writes), writes) // project mode never moves Jira from open/closed
	// The project's "Item closed" workflow sets Done; project sync carries it to Jira.
	x.setOption("Done")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"transition", "Finish"})
	s, entries := x.ok(s)
	eq(t, kinds(entries)[0], KindStatusAgreed)
	eq(t, []string{agreed(t, s), agreedOption(s)}, []string{"closed", "Done"})
}

func TestProjectStatusBlocksForReview(t *testing.T) {
	for name, change := range map[string]func(x *syncer){
		"is not mapped in project_status": func(x *syncer) { x.api.jira.Fields.Status = jiraStatus("Waiting", "indeterminate") },
		`"Blocked" is not mapped`:         func(x *syncer) { x.setOption("Blocked") },
		"Both sides changed": func(x *syncer) {
			x.setOption("In Progress")
			x.api.jira.Fields.Status = jiraStatus("Done", "done")
		},
		"No Jira transition": func(x *syncer) {
			x.setOption("In Progress")
			x.api.transitions = x.api.transitions[2:]
		},
	} {
		t.Run(name, func(t *testing.T) {
			x := projectSyncer(t)
			s := projectInSync(t, x)
			change(x)
			writes := len(x.api.writes)
			_, entries := x.ok(s)
			e := projectEntry(t, x, entries)
			eq(t, e.Kind, KindBlocked)
			if !strings.Contains(e.Reason, name) {
				t.Fatalf("reason: %s", e.Reason)
			}
			if name != "Both sides changed" { // there, open/closed legitimately closes GitHub
				eq(t, len(x.api.writes), writes)
			}
		})
	}
}

func TestNamedTransitionOverridesDestinationMatch(t *testing.T) {
	x := projectSyncer(t)
	x.c.ProjectStatus.Map[1].Transition = "Review"
	s := projectInSync(t, x)
	x.setOption("In Progress")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"transition", "Review"})
	eq(t, agreedOption(s), "In Progress")
}

func TestItemsOutsideProjectAreExcluded(t *testing.T) {
	x := projectSyncer(t)
	x.c.ProjectStatus.AddToProject = false
	s := x.linked()
	_, entries := x.ok(s)
	e := projectEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Reason}, []any{KindExcluded, "Issue is not in the project"})

	x.api.project.Items[42] = ProjectItem{ID: 900, OptionID: "o-todo", Archived: true}
	_, entries = x.ok(s)
	eq(t, projectEntry(t, x, entries).Reason, "Project item is archived")
}

func TestMappedOptionMissingFromProjectFailsRun(t *testing.T) {
	x := projectSyncer(t)
	s := x.linked()
	delete(x.api.project.Options, "Done")
	if _, err := Plan(x.c, s, x.api, time.Now()); err == nil || !strings.Contains(err.Error(), `"Done"`) {
		t.Fatalf("expected configuration error, got %v", err)
	}
}

func TestProjectStatusChangedAfterPlanAborts(t *testing.T) {
	x := projectSyncer(t)
	s := projectInSync(t, x)
	x.setOption("In Progress")
	entries, err := Plan(x.c, s, x.api, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	x.setOption("Done")
	writes := len(x.api.writes)
	if _, err := Apply(x.c, s, x.api, entries, x.save); err == nil {
		t.Fatal("expected abort")
	}
	eq(t, len(x.api.writes), writes)
}

func TestProjectStatusConfig(t *testing.T) {
	ps := Object{"owner": "cccteam", "project_number": 7, "map": []any{
		Object{"project": "Todo", "jira": "To Do"},
		Object{"project": "In Progress", "jira": []any{"In Progress", "In Review"}, "transition": "Start"}}}
	c := testConfig(t, Object{"project_status": ps})
	eq(t, []any{c.ProjectStatus.OwnerType, c.ProjectStatus.Field, c.ProjectStatus.AddToProject}, []any{"org", "Status", true})
	eq(t, c.ProjectStatus.Map[1], ProjectStatusMapping{Project: "In Progress", Jira: []string{"In Progress", "In Review"}, Transition: "Start"})
	option, _ := c.ProjectStatus.forJira("In Review")
	eq(t, option, "In Progress")

	base := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net", "jira_project": "ENG", "jira_issue_type": "10001"}
	withPS := func(change func(Object)) Object {
		raw := Object{"project_status": decodeJSON(canonical(ps, false))}
		for k, v := range base {
			raw[k] = v
		}
		change(raw)
		return raw
	}
	for name, change := range map[string]func(Object){
		"both modes":       func(r Object) { r["status_sync"] = Object{"close_transition": "Done", "reopen_transition": "Reopen"} },
		"no owner":         func(r Object) { delete(asObject(r["project_status"]), "owner") },
		"bad owner type":   func(r Object) { asObject(r["project_status"])["owner_type"] = "team" },
		"zero number":      func(r Object) { asObject(r["project_status"])["project_number"] = 0 },
		"unknown key":      func(r Object) { asObject(r["project_status"])["colour"] = "red" },
		"empty map":        func(r Object) { asObject(r["project_status"])["map"] = []any{} },
		"duplicate jira":   func(r Object) { list(asObject(r["project_status"])["map"])[1].(Object)["jira"] = "To Do" },
		"duplicate option": func(r Object) { list(asObject(r["project_status"])["map"])[1].(Object)["project"] = "Todo" },
		"no jira":          func(r Object) { delete(list(asObject(r["project_status"])["map"])[0].(Object), "jira") },
	} {
		if _, err := ParseConfig(canonical(withPS(change), false)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProjectAPIRequests(t *testing.T) {
	var srv *server
	srv = newServer(t, func(r *http.Request) (int, http.Header, any) {
		if r.Header.Get("X-GitHub-Api-Version") != "2026-03-10" || r.Header.Get("Authorization") != "Bearer project-token" {
			return 401, nil, nil
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/fields"):
			return 200, nil, []any{Object{"id": 5, "name": "Title", "data_type": "title"},
				Object{"id": 9, "name": "Status", "data_type": "single_select", "options": []any{
					Object{"id": "o-todo", "name": Object{"raw": "Todo", "html": "Todo"}},
					Object{"id": "o-done", "name": Object{"raw": "Done", "html": "Done"}}}}}
		case strings.HasSuffix(r.URL.Path, "/items") && r.Method == "GET" && r.URL.Query().Get("after") == "":
			next := http.Header{"Link": {`<` + srv.instance.URL + r.URL.Path + `?after=c1&fields=9>; rel="next"`}}
			return 200, next, []any{
				Object{"id": 900, "content_type": "Issue", "content": Object{"id": 42, "number": 1},
					"fields": []any{Object{"id": 9, "value": Object{"id": "o-done", "name": Object{"raw": "Done"}}}}},
				Object{"id": 901, "content_type": "DraftIssue", "content": Object{"id": 7}}}
		case strings.HasSuffix(r.URL.Path, "/items") && r.Method == "GET":
			return 200, nil, []any{Object{"id": 902, "content_type": "Issue", "content": Object{"id": 43},
				"archived_at": "2026-01-01T00:00:00Z", "fields": []any{Object{"id": 9, "value": "o-todo"}}}}
		case strings.HasSuffix(r.URL.Path, "/items/900"):
			return 200, nil, Object{"id": 900, "content_type": "Issue", "fields": []any{Object{"id": 9, "value": nil}}}
		}
		return 200, nil, nil
	})
	api := srv.client(t)
	api.c.ProjectStatus = &ProjectStatus{Owner: "acme", OwnerType: "org", ProjectNumber: 3, Field: "Status"}
	api.projects = srv.http()
	api.projects.headers = map[string]string{"Authorization": "Bearer project-token", "X-GitHub-Api-Version": "2026-03-10"}

	snap, err := api.Project()
	if err != nil {
		t.Fatal(err)
	}
	eq(t, snap.FieldID, int64(9))
	eq(t, snap.Items, map[int64]ProjectItem{42: {ID: 900, OptionID: "o-done"}, 43: {ID: 902, OptionID: "o-todo", Archived: true}})
	if !strings.HasPrefix(srv.urls[0], "/orgs/acme/projectsV2/3/fields") || !strings.Contains(srv.urls[1], "fields=9") {
		t.Fatalf("urls: %v", srv.urls)
	}
	item, err := api.ProjectItem(900, 9)
	if err != nil || item != (ProjectItem{ID: 900}) {
		t.Fatalf("unset item: %+v %v", item, err)
	}
	if err := api.AddProjectItem(1); err != nil {
		t.Fatal(err)
	}
	eq(t, string(canonical(srv.bodies[len(srv.bodies)-1], false)), `{"number":1,"owner":"acme","repo":"test","type":"Issue"}`)
	if err := api.SetProjectStatus(900, 9, "o-done"); err != nil {
		t.Fatal(err)
	}
	eq(t, string(canonical(srv.bodies[len(srv.bodies)-1], false)), `{"fields":[{"id":9,"value":"o-done"}]}`)
	eq(t, srv.urls[len(srv.urls)-1], "/orgs/acme/projectsV2/3/items/900")

	api.c.ProjectStatus.OwnerType = "user"
	eq(t, api.projectPath(), "/users/acme/projectsV2/3")
}

func TestPaginationRefusesForeignHosts(t *testing.T) {
	srv := newServer(t, func(*http.Request) (int, http.Header, any) {
		return 200, http.Header{"Link": {`<https://evil.example/next>; rel="next"`}}, []any{}
	})
	var out []any
	if _, err := srv.http().DoPage("/x", &out); err == nil {
		t.Fatal("followed a link to another host")
	}
}

func TestProjectTokenRequired(t *testing.T) {
	for k, v := range map[string]string{"GH_TOKEN": "x", "JIRA_EMAIL": "x", "JIRA_API_TOKEN": "x", "GH_PROJECT_TOKEN": ""} {
		t.Setenv(k, v)
	}
	c := testConfig(t, nil)
	c.ProjectStatus = &ProjectStatus{Owner: "acme", OwnerType: "org", ProjectNumber: 1, Field: "Status"}
	if _, err := NewClient(c); err == nil || !strings.Contains(err.Error(), "GH_PROJECT_TOKEN") {
		t.Fatalf("expected missing token error, got %v", err)
	}
	os.Setenv("GH_PROJECT_TOKEN", "p")
	client, err := NewClient(c)
	if err != nil || client.projects.headers["X-GitHub-Api-Version"] != "2026-03-10" {
		t.Fatalf("projects client: %v", err)
	}
	if client.github.headers["X-GitHub-Api-Version"] != "2022-11-28" {
		t.Fatal("issue client API version changed")
	}
}

// TestProjectAutomationRaceRecordsAgreement replays a live run: Jira moved to Done,
// so the tool closed the GitHub issue, and the project's "Item closed" workflow set
// the card to Done before the tool's own card update ran.
func TestProjectAutomationRaceRecordsAgreement(t *testing.T) {
	x := projectSyncer(t)
	s := projectInSync(t, x)
	x.api.itemClosed = true
	x.api.jira.Fields.Status = jiraStatus("Done", "done")
	s, entries, err := x.run(s)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	eq(t, kinds(entries)[:2], []Kind{KindSyncStatus, KindSyncProjectStatus})
	eq(t, projectEntry(t, x, entries).Outcome, OutcomeAlreadyDone)
	eq(t, lastWrite(x.api), write{"state", "closed"}) // the card update was not repeated
	eq(t, []string{agreed(t, s), agreedOption(s)}, []string{"closed", "Done"})
	writes := len(x.api.writes)
	_, entries = x.ok(s)
	eq(t, len(x.api.writes), writes)
	if projectEntry(t, x, entries) != nil {
		t.Fatal("agreement was not recorded")
	}
}

func TestRefusedProjectAddExplainsMissingRepositoryAccess(t *testing.T) {
	x := projectSyncer(t)
	s := x.linked()
	x.api.refuse = []int{422}
	_, entries := x.ok(s)
	e := projectEntry(t, x, entries)
	eq(t, e.Outcome, OutcomeRejected)
	if !strings.Contains(e.Reason, "HTTP 422") || !strings.Contains(e.Reason, "GH_PROJECT_TOKEN needs Issues: read") {
		t.Fatalf("reason: %s", e.Reason)
	}
	x.api.refuse = []int{400}
	_, entries = x.ok(s)
	if strings.Contains(projectEntry(t, x, entries).Reason, "Issues: read") {
		t.Fatal("hint shown for an unrelated refusal")
	}
}
