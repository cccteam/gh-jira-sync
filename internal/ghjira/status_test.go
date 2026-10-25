package ghjira

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func enableStatus(x *syncer) {
	x.c.StatusSync = &StatusSync{CloseTransition: "Done", ReopenTransition: "Reopen"}
}

// statusEntries keeps only status decisions, so tests ignore comment entries.
func statusEntries(entries []*Entry) []*Entry {
	var out []*Entry
	for _, e := range entries {
		if e.Operation != "" && (e.Kind == KindSyncStatus || e.Kind == KindStatusAgreed || strings.HasPrefix(e.Reason, "GitHub is ") ||
			strings.HasPrefix(e.Reason, "Jira transition")) {
			out = append(out, e)
		}
	}
	return out
}

func agreed(t *testing.T, s *State) string {
	t.Helper()
	if c := s.Pairs["1"].Closed; c != nil {
		return statusName(*c)
	}
	return "none"
}

func TestStatusSyncDisabledByDefault(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	eq(t, agreed(t, s), "none")
	x.api.source.State = "closed"
	_, entries := x.ok(s)
	eq(t, len(statusEntries(entries)), 0)
	eq(t, len(x.api.writes), 1)
}

func TestCreationRecordsOpenAsAgreedStatus(t *testing.T) {
	x := newSyncer(t)
	enableStatus(x)
	eq(t, agreed(t, x.linked()), "open")
}

func TestGitHubCloseAndReopenMoveJira(t *testing.T) {
	x := newSyncer(t)
	enableStatus(x)
	s := x.linked()
	x.api.source.State = "closed"
	s, entries := x.ok(s)
	st := statusEntries(entries)
	eq(t, len(st), 1)
	eq(t, []any{st[0].Kind, st[0].Destination, st[0].Status}, []any{KindSyncStatus, SideJira, "closed"})
	eq(t, lastWrite(x.api), write{"transition", "Done"})
	eq(t, x.api.jira.done(), true)
	eq(t, agreed(t, s), "closed")
	writes := len(x.api.writes)
	s, entries = x.ok(s)
	eq(t, len(statusEntries(entries)), 0)
	eq(t, len(x.api.writes), writes)

	x.api.source.State = "open"
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"transition", "Reopen"})
	eq(t, x.api.jira.done(), false)
	eq(t, agreed(t, s), "open")
}

func TestJiraDoneClosesAndReopensGitHub(t *testing.T) {
	x := newSyncer(t)
	enableStatus(x)
	s := x.linked()
	x.api.jira.Fields.Status = jiraStatus("Won't Do", "done")
	s, entries := x.ok(s)
	st := statusEntries(entries)
	eq(t, []any{st[0].Kind, st[0].Destination, st[0].Status}, []any{KindSyncStatus, SideGitHub, "closed"})
	eq(t, lastWrite(x.api), write{"state", "closed"})
	eq(t, agreed(t, s), "closed")

	x.api.jira.Fields.Status = jiraStatus("In Progress", "indeterminate")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"state", "open"})
	eq(t, agreed(t, s), "open")
}

// linkedBeforeStatus links a pair with status sync off, so no agreed status exists.
func linkedBeforeStatus(x *syncer) *State {
	s := x.linked()
	enableStatus(x)
	return s
}

func TestAgreementRecordedWithoutWrites(t *testing.T) {
	x := newSyncer(t)
	s := linkedBeforeStatus(x)
	s, entries := x.ok(s)
	st := statusEntries(entries)
	eq(t, []any{st[0].Kind, st[0].Status}, []any{KindStatusAgreed, "open"})
	eq(t, len(x.api.writes), 1)
	eq(t, agreed(t, s), "open")
}

func TestDisagreementWithoutAgreedStatusBlocksUntilAligned(t *testing.T) {
	x := newSyncer(t)
	s := linkedBeforeStatus(x)
	x.api.source.State = "closed"
	s, entries := x.ok(s)
	st := statusEntries(entries)
	eq(t, st[0].Kind, KindBlocked)
	if !strings.Contains(st[0].Reason, "no agreed status recorded") {
		t.Fatalf("reason: %s", st[0].Reason)
	}
	eq(t, len(x.api.writes), 1)
	acked, err := Acknowledge(s, entries, []string{st[0].Operation})
	if err != nil || st[0].Kind != KindAcknowledged {
		t.Fatalf("acknowledge: %v %s", err, st[0].Kind)
	}
	// Someone aligns Jira by hand; the agreement becomes the recorded status.
	x.api.jira.Fields.Status = jiraStatus("Done", "done")
	s, entries = x.ok(acked)
	eq(t, statusEntries(entries)[0].Kind, KindStatusAgreed)
	eq(t, agreed(t, s), "closed")
}

func TestMissingOrWrongTransitionBlocks(t *testing.T) {
	for name, transitions := range map[string][]JiraTransition{
		"is not available": {{ID: "11", Name: "Reopen", To: jiraStatus("To Do", "new")}},
		"would leave":      {{ID: "21", Name: "Done", To: jiraStatus("In Review", "indeterminate")}},
	} {
		t.Run(name, func(t *testing.T) {
			x := newSyncer(t)
			enableStatus(x)
			s := x.linked()
			x.api.transitions = transitions
			x.api.source.State = "closed"
			s, entries := x.ok(s)
			st := statusEntries(entries)
			eq(t, st[0].Kind, KindBlocked)
			if !strings.Contains(st[0].Reason, name) {
				t.Fatalf("reason: %s", st[0].Reason)
			}
			eq(t, len(x.api.writes), 1)
			eq(t, agreed(t, s), "open")
		})
	}
}

func TestRejectedStatusChangeIsRetried(t *testing.T) {
	x := newSyncer(t)
	enableStatus(x)
	s := x.linked()
	x.api.source.State = "closed"
	x.api.refuse = []int{400}
	s, entries := x.ok(s)
	eq(t, statusEntries(entries)[0].Outcome, OutcomeRejected)
	eq(t, agreed(t, s), "open")
	s, _ = x.ok(s)
	eq(t, lastWrite(x.api), write{"transition", "Done"})
	eq(t, agreed(t, s), "closed")
}

func TestStatusChangedAfterPlanAborts(t *testing.T) {
	x := newSyncer(t)
	enableStatus(x)
	s := x.linked()
	x.api.source.State = "closed"
	entries, err := Plan(x.c, s, x.api, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	x.api.source.State = "open" // reopened again: no longer what was planned, nor the target
	if _, err := Apply(x.c, s, x.api, entries, x.save); err == nil {
		t.Fatal("expected abort")
	}
	eq(t, len(x.api.writes), 1)
}

func TestStatusAlreadyReachedRecordsAgreement(t *testing.T) {
	x := newSyncer(t)
	enableStatus(x)
	s := x.linked()
	x.api.source.State = "closed"
	entries, err := Plan(x.c, s, x.api, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Someone moves Jira to Done before apply writes: the target is already in place.
	x.api.jira.Fields.Status = jiraStatus("Done", "done")
	s, err = Apply(x.c, s, x.api, entries, x.save)
	if err != nil {
		t.Fatal(err)
	}
	e := statusEntries(entries)[0]
	eq(t, []any{e.Outcome, len(x.api.writes)}, []any{OutcomeAlreadyDone, 1})
	eq(t, agreed(t, s), "closed")
}

func TestStatusSyncedWhenCommentsOffOrPairQuiet(t *testing.T) {
	x := newSyncer(t)
	enableStatus(x)
	x.c.Comments = false
	s := x.linked()
	x.api.source.State = "closed"
	_, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{KindSyncStatus})

	y := newSyncer(t)
	enableStatus(y)
	s = quietPair(t, y)
	// Closing usually bumps updated_at; keep it fixed to prove status ignores quiet skipping.
	y.api.source.State = "closed"
	_, entries = y.ok(s)
	eq(t, kinds(entries), []Kind{KindSyncStatus, KindUnchanged})
	eq(t, lastWrite(y.api), write{"transition", "Done"})
}

func TestStatusWritesCountTowardBudget(t *testing.T) {
	x := newSyncer(t)
	enableStatus(x)
	s := x.linked()
	x.c.MaxOperations = 1
	x.api.source.State = "closed"
	x.api.rows[SideGitHub] = []Comment{ghComment(11, "one")}
	_, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{KindSyncStatus, KindDeferred})
}

func TestStatusSyncConfig(t *testing.T) {
	c := testConfig(t, Object{"status_sync": Object{"close_transition": "Done", "reopen_transition": "Reopen"}})
	eq(t, *c.StatusSync, StatusSync{CloseTransition: "Done", ReopenTransition: "Reopen"})
	eq(t, testConfig(t, Object{"status_sync": nil}).StatusSync, (*StatusSync)(nil))
	base := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net", "jira_project": "ENG", "jira_issue_type": "10001"}
	for _, bad := range []any{"Done", Object{"close_transition": "Done"}, Object{"close_transition": "Done", "reopen_transition": " "},
		Object{"close_transition": "Done", "reopen_transition": "Reopen", "extra": true}} {
		raw := Object{"status_sync": bad}
		for k, v := range base {
			raw[k] = v
		}
		if _, err := ParseConfig(canonical(raw, false)); err == nil {
			t.Errorf("status_sync %v accepted", bad)
		}
	}
}

func TestStatusAPIRequests(t *testing.T) {
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/transitions"):
			return 200, nil, Object{"transitions": []any{Object{"id": "31", "name": "Done",
				"to": Object{"name": "Done", "statusCategory": Object{"key": "done"}}}}}
		case r.Method == "GET":
			return 200, nil, Object{"id": "200", "key": "ENG-1", "fields": Object{"project": Object{"key": "ENG"},
				"status": Object{"name": "Done", "statusCategory": Object{"key": "done"}}}}
		case r.Method == "POST":
			return 204, nil, nil
		}
		return 200, nil, Object{}
	})
	api := srv.client(t)
	issue, err := api.JiraIssue("200")
	if err != nil || !issue.done() {
		t.Fatalf("status not decoded: %+v %v", issue, err)
	}
	if !strings.Contains(srv.urls[0], "fields=project%2Clabels%2Cupdated%2Cstatus") && !strings.Contains(srv.urls[0], "fields=project,labels,updated,status") {
		t.Fatalf("status field not requested: %s", srv.urls[0])
	}
	transitions, err := api.JiraTransitions("200")
	if err != nil || len(transitions) != 1 || transitions[0].ID != "31" || !transitions[0].To.done() {
		t.Fatalf("transitions: %+v %v", transitions, err)
	}
	if err := api.TransitionJira("200", "31"); err != nil {
		t.Fatal(err)
	}
	eq(t, canonical(srv.bodies[len(srv.bodies)-1], false), canonical(Object{"transition": Object{"id": "31"}}, false))
	for _, closed := range []bool{true, false} {
		if err := api.SetGitHubState(7, closed); err != nil {
			t.Fatal(err)
		}
		body := srv.bodies[len(srv.bodies)-1]
		eq(t, body["state"], any(statusName(closed)))
		if !slices.Contains([]any{"completed", "reopened"}, body["state_reason"]) {
			t.Fatalf("state_reason: %v", body["state_reason"])
		}
		if !strings.HasSuffix(srv.urls[len(srv.urls)-1], "/repos/acme/test/issues/7") {
			t.Fatalf("url: %s", srv.urls[len(srv.urls)-1])
		}
	}
}
