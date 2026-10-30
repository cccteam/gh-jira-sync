package ghjira

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tokenServer is a fake Atlassian token endpoint that answers with reply and counts requests.
func tokenServer(t *testing.T, code int, reply any) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.ParseForm() != nil ||
			r.PostForm.Get("grant_type") != "client_credentials" ||
			r.PostForm.Get("client_id") != "id" || r.PostForm.Get("client_secret") != "s3cret" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestOAuthTokenIsReusedThenRenewed(t *testing.T) {
	t.Parallel()
	srv, calls := tokenServer(t, 200, Object{"access_token": "abc", "token_type": "Bearer", "expires_in": 3600})
	o := newOAuthToken(srv.URL, "id", "s3cret")
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	o.now = func() time.Time { return now }

	for range 2 {
		header, err := o.header()
		if err != nil {
			t.Fatal(err)
		}
		eq(t, header, "Bearer abc")
	}
	eq(t, calls.Load(), int32(1))

	now = now.Add(54 * time.Minute) // still more than five minutes left
	if _, err := o.header(); err != nil {
		t.Fatal(err)
	}
	eq(t, calls.Load(), int32(1))

	now = now.Add(2 * time.Minute) // within five minutes of expiry
	if _, err := o.header(); err != nil {
		t.Fatal(err)
	}
	eq(t, calls.Load(), int32(2))
}

// A token failure is a refusal: the Jira request was never sent, so a creation is
// rejected and retried instead of left pending. Errors never carry the secret.
func TestOAuthTokenFailuresAreRefusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		code  int
		reply any
	}{
		"refused":      {401, Object{"error": "access_denied", "client_secret": "s3cret"}},
		"no token":     {200, Object{"token_type": "Bearer", "expires_in": 3600}},
		"wrong type":   {200, Object{"access_token": "abc", "token_type": "mac", "expires_in": 3600}},
		"no lifetime":  {200, Object{"access_token": "abc", "token_type": "Bearer"}},
		"server error": {503, nil},
	} {
		srv, _ := tokenServer(t, tc.code, tc.reply)
		_, err := newOAuthToken(srv.URL, "id", "s3cret").header()
		if status, refused := refusedStatus(err); !refused || status != 401 {
			t.Errorf("%s: got %v, want a 401 refusal", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: error leaks the secret: %v", name, err)
		}
	}
	srv, _ := tokenServer(t, 200, nil)
	srv.Close()
	if _, refused := refusedStatus(func() error { _, err := newOAuthToken(srv.URL, "id", "s3cret").header(); return err }()); !refused {
		t.Error("unreachable token endpoint not reported as a refusal")
	}
}

func TestJiraRequestsCarryOAuthToken(t *testing.T) {
	t.Parallel()
	var auth atomic.Value
	jira := newServer(t, func(r *http.Request) (int, http.Header, any) {
		auth.Store(r.Header.Get(headerAuthorization))
		return 200, nil, Object{}
	})
	h := jira.http()
	h.auth = func() (string, error) { return "Bearer abc", nil }
	if err := h.Do("GET", "/rest/api/3/serverInfo", nil, nil); err != nil {
		t.Fatal(err)
	}
	eq(t, auth.Load(), any("Bearer abc"))

	h.auth = func() (string, error) { return "", tokenErr("Jira OAuth token request failed") }
	if err := h.Do("POST", "/rest/api/3/issue", Object{}, nil); err == nil {
		t.Fatal("request sent without a token")
	}
	eq(t, len(jira.urls), 1)
}

func TestJiraCredentialChoice(t *testing.T) {
	gateway := testConfig(t, Object{"jira_cloud_id": testCloudID})
	site := testConfig(t, nil)
	for _, tc := range []struct {
		name                          string
		config                        *Config
		email, token, id, secret      string
		wantErr, wantOAuth, wantBasic bool
	}{
		{name: "api token", config: site, email: "e", token: "t", wantBasic: true},
		{name: "api token via gateway", config: gateway, email: "e", token: "t", wantBasic: true},
		{name: "oauth", config: gateway, id: "id", secret: "s", wantOAuth: true},
		{name: "oauth without cloud id", config: site, id: "id", secret: "s", wantErr: true},
		{name: "both", config: gateway, email: "e", token: "t", id: "id", secret: "s", wantErr: true},
		{name: "leftover email", config: gateway, email: "e", id: "id", secret: "s", wantErr: true},
		{name: "half oauth", config: gateway, id: "id", wantErr: true},
		{name: "half api token", config: site, email: "e", wantErr: true},
		{name: "none", config: site, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JIRA_EMAIL", tc.email)
			t.Setenv("JIRA_API_TOKEN", tc.token)
			t.Setenv("JIRA_CLIENT_ID", tc.id)
			t.Setenv("JIRA_CLIENT_SECRET", tc.secret)
			h, err := newJiraHTTP(tc.config)
			if tc.wantErr {
				if err == nil {
					t.Fatal("accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			eq(t, h.base, tc.config.JiraAPIURL())
			eq(t, h.auth != nil, tc.wantOAuth)
			eq(t, strings.HasPrefix(h.headers[headerAuthorization], "Basic "), tc.wantBasic)
		})
	}
}
