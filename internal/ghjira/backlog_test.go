package ghjira

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// backlogSyncer splits Jira's To Do by board backlog: Backlog while the issue is in
// the backlog, Ready once it is on the board. A board move lands in To Do, like a
// Kanban board's first column.
func backlogSyncer(t *testing.T) *syncer {
	x := projectSyncer(t)
	x.c.ProjectStatus.JiraBoard = 150
	x.c.ProjectStatus.Map = []ProjectStatusMapping{
		{Project: "Backlog", JiraBacklog: true},
		{Project: "Ready", Jira: []string{"To Do"}},
		{Project: "In Progress", Jira: []string{"In Progress", "In Review"}},
		{Project: "Done", Jira: []string{"Done"}},
	}
	x.api.project.Options = map[string]string{"Backlog": "o-backlog", "Ready": "o-ready", "In Progress": "o-prog", "Done": "o-done"}
	toDo := jiraStatus("To Do", "new")
	x.api.boardColumn = &toDo
	x.api.backlog = true
	return x
}

// backlogInSync links the issue and fills the project's Backlog from Jira's backlog.
func backlogInSync(t *testing.T, x *syncer) *State {
	t.Helper()
	s := x.linked()
	s, _ = x.ok(s) // add to the project
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"project", "Backlog"})
	eq(t, agreedOption(s), "Backlog")
	return s
}

func TestJiraBoardMoveSetsReady(t *testing.T) {
	x := backlogSyncer(t)
	s := backlogInSync(t, x)
	x.api.backlog = false
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"project", "Ready"})
	eq(t, agreedOption(s), "Ready")

	// Back to the backlog in Jira, whatever the status.
	x.api.jira.Fields.Status = jiraStatus("In Progress", "indeterminate")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"project", "In Progress"})
	x.api.backlog = true
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"project", "Backlog"})
	eq(t, agreedOption(s), "Backlog")
}

func TestProjectReadyMovesJiraOntoBoard(t *testing.T) {
	x := backlogSyncer(t)
	s := backlogInSync(t, x)
	x.setOption("Ready")
	writes := len(x.api.writes)
	s, entries := x.ok(s)
	e := projectEntry(t, x, entries)
	eq(t, []any{e.Kind, e.Destination, e.Status}, []any{KindSyncProjectStatus, SideJira, "Ready"})
	eq(t, x.api.writes[writes:], []write{{"board", x.api.jira.Key}}) // To Do already maps to Ready
	eq(t, agreedOption(s), "Ready")

	x.setOption("Backlog")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"backlog", x.api.jira.Key})
	eq(t, []any{x.api.backlog, agreedOption(s)}, []any{true, "Backlog"})
}

func TestProjectInProgressMovesJiraOntoBoardThenTransitions(t *testing.T) {
	x := backlogSyncer(t)
	s := backlogInSync(t, x)
	x.setOption("In Progress")
	writes := len(x.api.writes)
	s, _ = x.ok(s)
	eq(t, x.api.writes[writes:], []write{{"board", x.api.jira.Key}, {"transition", "Start"}})
	eq(t, []any{x.api.backlog, x.api.jira.Fields.Status.Name, agreedOption(s)}, []any{false, "In Progress", "In Progress"})
}

// The board move can change the status; the transition is found again from there.
func TestBoardMoveStatusChangeRedoesTransition(t *testing.T) {
	x := backlogSyncer(t)
	s := backlogInSync(t, x)
	x.api.jira.Fields.Status = jiraStatus("In Review", "indeterminate") // in the backlog, so still Backlog
	s, _ = x.ok(s)
	eq(t, agreedOption(s), "Backlog")
	x.setOption("Done")
	writes := len(x.api.writes)
	s, _ = x.ok(s)
	eq(t, x.api.writes[writes:writes+2], []write{{"board", x.api.jira.Key}, {"transition", "Finish"}})
	eq(t, []any{x.api.jira.Fields.Status.Name, agreedOption(s)}, []any{"Done", "Done"})
}

func TestDoneIssueCannotMoveToBacklog(t *testing.T) {
	x := backlogSyncer(t)
	s := backlogInSync(t, x)
	x.api.backlog = false
	x.api.jira.Fields.Status = jiraStatus("Done", "done")
	s, _ = x.ok(s)
	eq(t, agreedOption(s), "Done")
	x.setOption("Backlog")
	writes := len(x.api.writes)
	_, entries := x.ok(s)
	e := projectEntry(t, x, entries)
	eq(t, e.Kind, KindBlocked)
	if !strings.Contains(e.Reason, "only incomplete issues") || e.Destination != "" {
		t.Fatalf("entry: %+v", e)
	}
	eq(t, len(x.api.writes), writes)
}

func TestBacklogChangedAfterPlanAborts(t *testing.T) {
	x := backlogSyncer(t)
	s := backlogInSync(t, x)
	x.setOption("Ready")
	entries, err := Plan(x.c, s, x.api, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	x.api.jira.Fields.Status = jiraStatus("In Progress", "indeterminate")
	x.api.backlog = false
	writes := len(x.api.writes)
	if _, err := Apply(x.c, s, x.api, entries, x.save); err == nil {
		t.Fatal("expected abort")
	}
	eq(t, len(x.api.writes), writes)
}

func TestBacklogMoveAlreadyDone(t *testing.T) {
	x := backlogSyncer(t)
	s := backlogInSync(t, x)
	x.setOption("Ready")
	entries, err := Plan(x.c, s, x.api, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	x.api.backlog = false // someone dragged it onto the board meanwhile
	writes := len(x.api.writes)
	s, err = Apply(x.c, s, x.api, entries, x.save)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, projectEntry(t, x, entries).Outcome, OutcomeAlreadyDone)
	eq(t, []any{len(x.api.writes), agreedOption(s)}, []any{writes, "Ready"})
}

func TestBacklogConfig(t *testing.T) {
	ps := func() Object {
		return Object{"owner": "cccteam", "project_number": 7, "jira_board": 150, "map": []any{
			Object{"project": "Backlog", "jira_backlog": true},
			Object{"project": "Ready", "jira": "To Do"}}}
	}
	c := testConfig(t, Object{"project_status": ps()})
	eq(t, []any{c.ProjectStatus.JiraBoard, c.ProjectStatus.backlogOption()}, []any{150, "Backlog"})
	for _, inBacklog := range []bool{true, false} {
		option, _ := c.ProjectStatus.optionFor("To Do", inBacklog)
		eq(t, option, map[bool]string{true: "Backlog", false: "Ready"}[inBacklog])
	}

	for name, change := range map[string]func(Object){
		"no board":           func(p Object) { delete(p, "jira_board") },
		"board, no backlog":  func(p Object) { list(p["map"])[0] = Object{"project": "Backlog", "jira": "Backlog"} },
		"zero board":         func(p Object) { p["jira_board"] = 0 },
		"backlog with jira":  func(p Object) { list(p["map"])[0].(Object)["jira"] = "To Do" },
		"backlog transition": func(p Object) { list(p["map"])[0].(Object)["transition"] = "Start" },
		"two backlogs":       func(p Object) { list(p["map"])[1] = Object{"project": "Ready", "jira_backlog": true} },
		"not a bool":         func(p Object) { list(p["map"])[0].(Object)["jira_backlog"] = "yes" },
	} {
		raw := ps()
		change(raw)
		if _, err := ParseConfig(canonical(Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
			"jira_project": "ENG", "jira_issue_type": "10001", "project_status": raw}, false)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBacklogAPIRequests(t *testing.T) {
	inBacklog := map[string]int{"10010": 1, "ENG-1": 0} // what membership reads return
	lag := 0                                            // reads still showing the old membership
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		switch {
		case r.URL.Path == "/rest/agile/1.0/board/150/backlog" && r.URL.Query().Get("jql") != "":
			total := inBacklog[strings.TrimPrefix(r.URL.Query().Get("jql"), "issue = ")]
			if lag > 0 {
				lag--
				total = 1 - total
			}
			return 200, nil, Object{"startAt": 0, "total": total, "issues": []any{}}
		case r.URL.Path == "/rest/agile/1.0/backlog/150/issue":
			inBacklog["ENG-1"], lag = 1, 2
		case r.URL.Path == "/rest/agile/1.0/board/150/issue":
			inBacklog["ENG-1"] = 0
		case r.URL.Path == "/rest/agile/1.0/board/150/backlog" && r.URL.Query().Get("startAt") == "0":
			return 200, nil, Object{"startAt": 0, "total": 3, "issues": []any{Object{"id": "1"}, Object{"id": "2"}}}
		case r.URL.Path == "/rest/agile/1.0/board/150/backlog":
			return 200, nil, Object{"startAt": 2, "total": 3, "issues": []any{Object{"id": "3"}}}
		case r.URL.Path == "/rest/agile/1.0/board/151/backlog":
			return 400, nil, Object{"errorMessages": []any{"The board does not support backlog"}}
		}
		return 204, nil, nil
	})
	api := srv.client(t)
	api.c.ProjectStatus = &ProjectStatus{JiraBoard: 150}
	ids, err := api.BacklogIssues()
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids, map[string]bool{"1": true, "2": true, "3": true})
	if !strings.Contains(srv.urls[1], "startAt=2") || !strings.Contains(srv.urls[1], "fields=id") {
		t.Fatalf("urls: %v", srv.urls)
	}
	in, err := api.InBacklog("10010")
	if err != nil || !in {
		t.Fatalf("in backlog: %v %v", in, err)
	}
	if !strings.Contains(srv.urls[2], "jql=issue+%3D+10010") {
		t.Fatalf("url: %s", srv.urls[2])
	}
	// The move waits out the listing's lag: two stale reads, then the moved issue.
	if err := api.MoveToBacklog("ENG-1"); err != nil {
		t.Fatal(err)
	}
	eq(t, srv.urls[3], "/rest/agile/1.0/backlog/150/issue")
	eq(t, string(canonical(srv.bodies[3], false)), `{"issues":["ENG-1"]}`)
	eq(t, []any{len(srv.urls), len(srv.sleeps)}, []any{7, 2})
	if err := api.MoveToBoard("ENG-1"); err != nil {
		t.Fatal(err)
	}
	eq(t, srv.urls[7], "/rest/agile/1.0/board/150/issue")
	eq(t, string(canonical(srv.bodies[7], false)), `{"issues":["ENG-1"]}`)
	eq(t, []any{len(srv.urls), len(srv.sleeps)}, []any{9, 2})

	api.c.ProjectStatus.JiraBoard = 151
	if _, err := api.BacklogIssues(); err == nil || !strings.Contains(err.Error(), "has no backlog") {
		t.Fatalf("expected a backlog-disabled error, got %v", err)
	}
}
