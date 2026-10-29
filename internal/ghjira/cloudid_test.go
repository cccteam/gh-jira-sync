package ghjira

import (
	"net/http"
	"strings"
	"testing"
)

const testCloudID = "0d1e2f3a-4b5c-6d7e-8f90-a1b2c3d4e5f6"

// An Atlassian service account calls Jira through the API gateway; links and the
// deployment binding stay on the site, so switching keeps the existing state valid.
func TestCloudIDRoutesAPIThroughGateway(t *testing.T) {
	site := testConfig(t, nil)
	gateway := testConfig(t, Object{"jira_cloud_id": strings.ToUpper(testCloudID)})
	eq(t, site.JiraAPIURL(), "https://acme.atlassian.net")
	eq(t, gateway.JiraCloudID, testCloudID)
	eq(t, gateway.JiraAPIURL(), "https://api.atlassian.com/ex/jira/"+testCloudID)
	eq(t, binding(gateway), binding(site))
	if validateState(NewState(site), gateway) != nil {
		t.Fatal("state from a site deployment rejected after switching to the gateway")
	}

	for k, v := range map[string]string{"GH_TOKEN": "x", "JIRA_EMAIL": "x", "JIRA_API_TOKEN": "x", "JIRA_CLIENT_ID": "", "JIRA_CLIENT_SECRET": ""} {
		t.Setenv(k, v)
	}
	client, err := NewClient(gateway)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, client.jira.base, "https://api.atlassian.com/ex/jira/"+testCloudID)

	prefix, _ := envelope(gateway, Comment{Side: SideJira, ID: "7", Author: "Ada"}, Pair{JiraKey: "ENG-1"}, "op")
	if !strings.Contains(prefix, "https://acme.atlassian.net/browse/ENG-1?focusedCommentId=7") {
		t.Fatalf("comment link left the site: %q", prefix)
	}
	body, err := githubBody(gateway, &JiraIssue{Key: "ENG-1"}, "op")
	if err != nil || !strings.HasPrefix(body, "From Jira: https://acme.atlassian.net/browse/ENG-1") {
		t.Fatalf("issue link left the site: %q, %v", body, err)
	}
}

func TestCloudIDMustBeAUUID(t *testing.T) {
	t.Parallel()
	for _, bad := range []any{"acme", "https://api.atlassian.com/ex/jira/" + testCloudID, testCloudID + "0", 5, true} {
		raw := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
			"jira_project": "ENG", "jira_issue_type": "10001", "jira_cloud_id": bad}
		if _, err := ParseConfig(canonical(raw, false)); err == nil {
			t.Errorf("jira_cloud_id %v accepted", bad)
		}
	}
	if c := testConfig(t, Object{"jira_cloud_id": nil}); c.JiraCloudID != "" {
		t.Fatal("null jira_cloud_id should leave the gateway off")
	}
}

// A cloud ID for another site would write there while links point at jira_url.
func TestCheckProjectConfirmsCloudIDSite(t *testing.T) {
	t.Parallel()
	jira := func(baseURL string) *server {
		return newServer(t, func(r *http.Request) (int, http.Header, any) {
			if r.URL.Path == "/rest/api/3/serverInfo" {
				return 200, nil, Object{"baseUrl": baseURL}
			}
			return 200, nil, Object{"projectTypeKey": "software", "issueTypes": []any{Object{"id": "10001", "subtask": false}}}
		})
	}
	withCloudID := func(srv *server) *Client {
		api := srv.client(t)
		api.c = testConfig(t, Object{"jira_cloud_id": testCloudID})
		return api
	}

	match := jira("https://ACME.atlassian.net/")
	if err := withCloudID(match).CheckProject(); err != nil {
		t.Fatalf("matching site rejected: %v", err)
	}
	eq(t, match.urls[0], "/rest/api/3/serverInfo")

	if err := withCloudID(jira("https://other.atlassian.net")).CheckProject(); err == nil ||
		!strings.Contains(err.Error(), "other.atlassian.net") {
		t.Fatalf("cloud ID for another site accepted: %v", err)
	}

	direct := jira("https://other.atlassian.net")
	if err := direct.client(t).CheckProject(); err != nil {
		t.Fatalf("site deployment: %v", err)
	}
	for _, u := range direct.urls {
		if strings.Contains(u, "serverInfo") {
			t.Fatal("site deployment checked serverInfo")
		}
	}
}
