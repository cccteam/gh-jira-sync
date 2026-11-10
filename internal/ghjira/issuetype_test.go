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

	base := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net", "jira_project": "ENG", "jira_default_issue_type": "10001"}
	for _, bad := range []any{
		"10300",
		Object{"Feature": 10300},
		Object{"Feature": ""},
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

// TestLegacyIssueTypeKey keeps configurations written before jira_default_issue_type
// working, and rejects setting both names.
func TestLegacyIssueTypeKey(t *testing.T) {
	t.Parallel()
	legacy := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net", "jira_project": "ENG", "jira_issue_type": "Task"}
	c, err := ParseConfig(canonical(legacy, false))
	if err != nil {
		t.Fatalf("legacy jira_issue_type rejected: %v", err)
	}
	eq(t, c.JiraDefaultIssueType, "Task")
	legacy["jira_default_issue_type"] = "Story"
	if _, err := ParseConfig(canonical(legacy, false)); err == nil || !strings.Contains(err.Error(), "old name") {
		t.Errorf("both names accepted, got %v", err)
	}
	delete(legacy, "jira_issue_type")
	delete(legacy, "jira_default_issue_type")
	if _, err := ParseConfig(canonical(legacy, false)); err == nil {
		t.Error("config without a default issue type accepted")
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

	// A type change between plan and apply would create the wrong type, so it waits a run.
	x.api.source.Type = typed("Bug").Type
	_, err = Apply(x.c, NewState(x.c), x.api, entries, x.save)
	wantRetry(t, entries, err, "GitHub issue changed")
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
		t.Error("missing jira_default_issue_type accepted")
	}
}

func TestCheckProjectResolvesIssueTypeNames(t *testing.T) {
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
	task := Object{"id": "10001", "name": "Task", "subtask": false}
	story := Object{"id": "10300", "name": "Story", "subtask": false}
	subtask := Object{"id": "10302", "name": "Subtask", "subtask": true}

	api := project(task, story, subtask).client(t)
	api.c.JiraDefaultIssueType, api.c.JiraSubtaskType = "task", "Subtask"
	api.c.JiraIssueTypes = map[string]string{"Feature": "STORY", "Bug": "10001"}
	if err := api.CheckProject(); err != nil {
		t.Fatalf("valid names rejected: %v", err)
	}
	eq(t, []any{api.c.JiraDefaultIssueType, api.c.JiraSubtaskType, api.c.JiraIssueTypes["Feature"], api.c.JiraIssueTypes["Bug"]},
		[]any{"10001", "10302", "10300", "10001"})
	eq(t, api.c.githubTypeFor("10300"), "Feature")

	for name, tc := range map[string]struct {
		types []Object
		set   func(*Config)
		want  string
	}{
		"unknown name":         {[]Object{task}, func(c *Config) { c.JiraIssueTypes = map[string]string{"Feature": "Epic"} }, `Epic is not a standard`},
		"sub-task as standard": {[]Object{task, subtask}, func(c *Config) { c.JiraDefaultIssueType = "Subtask" }, `Subtask is not a standard`},
		"standard as sub-task": {[]Object{task}, func(c *Config) { c.JiraSubtaskType = "Task" }, `jira_subtask_type Task`},
		"ambiguous name": {
			[]Object{task, {"id": "10400", "name": "TASK", "subtask": false}},
			func(c *Config) { c.JiraDefaultIssueType = "Task" }, `matches 2 issue types`,
		},
		// Digits are IDs only, so a type named like another's ID is never picked.
		"digits match IDs only": {
			[]Object{task, {"id": "10500", "name": "10001", "subtask": true}},
			func(c *Config) { c.JiraDefaultIssueType = "10001" }, "",
		},
	} {
		api := project(tc.types...).client(t)
		tc.set(api.c)
		err := api.CheckProject()
		if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want))) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}
