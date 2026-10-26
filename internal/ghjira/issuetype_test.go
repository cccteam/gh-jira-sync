package ghjira

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func typed(name string) *GitHubIssue {
	issue := &GitHubIssue{ID: 42, Number: 1}
	if name != "" {
		issue.Type = &struct {
			Name string `json:"name"`
		}{name}
	}
	return issue
}

func TestIssueTypeMapping(t *testing.T) {
	t.Parallel()
	c := testConfig(t, Object{"jira_issue_types": Object{"Feature": "10300", "bug": "10301"}})
	for name, want := range map[string]string{"Feature": "10300", "Bug": "10301", "BUG": "10301", "Task": "10001", "": "10001"} {
		if got := c.issueTypeFor(typed(name)); got != want {
			t.Errorf("type %q: got %s, want %s", name, got, want)
		}
	}
	eq(t, len(testConfig(t, Object{"jira_issue_types": nil}).JiraIssueTypes), 0)

	base := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net", "jira_project": "ENG", "jira_issue_type": "10001"}
	for _, bad := range []any{
		"10300",
		Object{"Feature": 10300},
		Object{"Feature": "Story"},
		Object{" ": "10300"},
		Object{"Bug": "1", "bug": "2"},
	} {
		raw := Object{"jira_issue_types": bad}
		for k, v := range base {
			raw[k] = v
		}
		if _, err := ParseConfig(canonical(raw, false)); err == nil {
			t.Errorf("jira_issue_types %v accepted", bad)
		}
	}
}

func TestPlannedCreationShowsMappedType(t *testing.T) {
	t.Parallel()
	x := newSyncer(t)
	x.c.JiraIssueTypes = map[string]string{"Feature": "10300"}
	x.api.source.Type = typed("Feature").Type
	entries, err := Plan(x.c, NewState(x.c), x.api, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	eq(t, []any{entries[0].Kind, entries[0].IssueType}, []any{KindCreateIssue, "10300"})

	// A type change between plan and apply would create the wrong type, so it aborts.
	x.api.source.Type = typed("Bug").Type
	if _, err := Apply(x.c, NewState(x.c), x.api, entries, x.save); err == nil {
		t.Fatal("created an issue whose GitHub type changed after plan")
	}
	eq(t, len(x.api.writes), 0)
}

func TestCreateIssueUsesMappedType(t *testing.T) {
	t.Parallel()
	srv := newServer(t, ok(Object{"id": "200", "key": "ENG-1"}))
	api := srv.client(t)
	api.c.JiraIssueTypes = map[string]string{"Feature": "10300"}
	for _, tc := range []struct{ github, jira string }{{"Feature", "10300"}, {"Bug", "10001"}, {"", "10001"}} {
		source := newFake().source
		source.Type = typed(tc.github).Type
		if _, err := api.CreateIssue(&source, ""); err != nil {
			t.Fatal(err)
		}
		fields := asObject(srv.bodies[len(srv.bodies)-1]["fields"])
		eq(t, asObject(fields["issuetype"])["id"], any(tc.jira))
	}
}

func TestCheckProjectValidatesIssueTypes(t *testing.T) {
	t.Parallel()
	project := func(types ...Object) *server {
		list := make([]any, len(types))
		for i, ty := range types {
			list[i] = ty
		}
		return newServer(t, func(*http.Request) (int, http.Header, any) {
			return 200, nil, Object{"projectTypeKey": "software", "issueTypes": list}
		})
	}
	task, story := Object{"id": "10001", "subtask": false}, Object{"id": "10300", "subtask": false}
	subtask := Object{"id": "10302", "subtask": true}

	api := project(task, story, subtask).client(t)
	api.c.JiraIssueTypes = map[string]string{"Feature": "10300"}
	if err := api.CheckProject(); err != nil {
		t.Fatalf("valid types rejected: %v", err)
	}
	for name, mapping := range map[string]string{"missing": "10999", "subtask": "10302"} {
		api := project(task, story, subtask).client(t)
		api.c.JiraIssueTypes = map[string]string{"Feature": mapping}
		if err := api.CheckProject(); err == nil || !strings.Contains(err.Error(), mapping) {
			t.Errorf("%s type: got %v", name, err)
		}
	}
	if err := project(story).client(t).CheckProject(); err == nil {
		t.Error("missing default jira_issue_type accepted")
	}
}
