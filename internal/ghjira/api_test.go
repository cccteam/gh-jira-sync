package ghjira

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// server answers requests through respond and records each request URL and body.
type server struct {
	mu       sync.Mutex
	urls     []string
	bodies   []Object
	sleeps   []time.Duration
	respond  func(r *http.Request) (int, http.Header, any)
	instance *httptest.Server
}

func newServer(t *testing.T, respond func(r *http.Request) (int, http.Header, any)) *server {
	srv := &server{respond: respond}
	srv.instance = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		srv.mu.Lock()
		srv.urls = append(srv.urls, r.URL.RequestURI())
		var parsed Object
		if len(body) > 0 {
			parsed, _ = decodeJSON(body).(Object)
		}
		srv.bodies = append(srv.bodies, parsed)
		srv.mu.Unlock()
		status, headers, payload := srv.respond(r)
		maps.Copy(w.Header(), headers)
		w.WriteHeader(status)
		if raw, isRaw := payload.([]byte); isRaw {
			_, _ = w.Write(raw)
		} else if payload != nil {
			_ = json.NewEncoder(w).Encode(payload)
		}
	}))
	t.Cleanup(srv.instance.Close)
	return srv
}

func (s *server) http() *HTTP {
	h := newHTTP(s.instance.URL, map[string]string{})
	h.sleep = func(d time.Duration) { s.sleeps = append(s.sleeps, d) }
	return h
}

// client points both services at one test server.
func (s *server) client(t *testing.T) *Client {
	return &Client{c: testConfig(t, nil), github: s.http(), jira: s.http()}
}

func ok(payload any) func(*http.Request) (int, http.Header, any) {
	return func(*http.Request) (int, http.Header, any) { return 200, nil, payload }
}

func status(code int, headers http.Header) func(*http.Request) (int, http.Header, any) {
	return func(*http.Request) (int, http.Header, any) { return code, headers, nil }
}

func TestWriteNotRetried(t *testing.T) {
	t.Parallel()
	srv := newServer(t, ok(nil))
	h := srv.http()
	srv.instance.Close() // every request now fails at the network level
	if err := h.Do("POST", "/issue", Object{}, nil); err == nil {
		t.Fatal("expected network failure")
	}
	eq(t, len(srv.sleeps), 0)
}

func TestReadRetries(t *testing.T) {
	t.Parallel()
	srv := newServer(t, ok(nil))
	h := srv.http()
	srv.instance.Close()
	if err := h.Do("GET", "/issue", nil, nil); err == nil {
		t.Fatal("expected network failure")
	}
	eq(t, srv.sleeps, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second})
}

func TestIssueSelectionAndPRFilter(t *testing.T) {
	t.Parallel()
	srv := newServer(t, ok([]any{Object{"number": 1}, Object{"number": 2, "pull_request": Object{}}}))
	issues, err := srv.client(t).Issues()
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(issues), 1)
	if !strings.Contains(srv.urls[0], "labels=sync-jira") {
		t.Fatalf("label filter missing: %s", srv.urls[0])
	}
}

func page(n, start int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = Object{"id": start + i}
	}
	return out
}

func TestGitHubPaginationAndLimit(t *testing.T) {
	t.Parallel()
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		if r.URL.Query().Get("page") == "1" {
			return 200, nil, page(100, 0)
		}
		return 200, nil, page(1, 100)
	})
	api := srv.client(t)
	rows, err := ghPages[Object](api, "/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(rows), 101)
	api.c.MaxPages = 1
	if _, err := ghPages[Object](api, "/test", nil); err == nil {
		t.Fatal("partial scan must fail")
	}
}

func TestJiraPaginationAndCommentProperties(t *testing.T) {
	t.Parallel()
	op := strings.Repeat("c", 64)
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		if r.URL.Path == "/rest/api/3/comment/list" {
			// Properties come only from this documented bulk endpoint.
			return 200, nil, Object{"isLast": true, "values": []any{
				Object{"id": "10", "properties": []any{Object{"key": "gh-jira-sync", "value": Object{"operation": op}}}},
				Object{"id": "11", "properties": []any{Object{"key": "sd.public.comment", "value": Object{"internal": true}}}},
			}}
		}
		start := r.URL.Query().Get("startAt")
		return 200, nil, Object{"comments": []any{Object{"id": "1" + start}}, "total": 2}
	})
	rows, err := srv.client(t).Comments(SideJira, Pair{JiraID: "200"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(rows), 2)
	if !strings.Contains(srv.urls[1], "startAt=1") {
		t.Fatalf("second page not requested from offset 1: %s", srv.urls[1])
	}
	eq(t, srv.urls[2], "/rest/api/3/comment/list?expand=properties")
	eq(t, string(canonical(srv.bodies[2], false)), `{"ids":[10,11]}`)
	eq(t, []any{rows[0].Identity, rows[0].Restricted, rows[1].Identity, rows[1].Restricted}, []any{op, false, "", true})
}

func TestPartialCommentPropertiesFail(t *testing.T) {
	t.Parallel()
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		if r.URL.Path == "/rest/api/3/comment/list" {
			return 200, nil, Object{"isLast": true, "values": []any{Object{"id": "10"}}}
		}
		return 200, nil, Object{"comments": []any{Object{"id": "10"}, Object{"id": "11"}}, "total": 2}
	})
	if _, err := srv.client(t).Comments(SideJira, Pair{JiraID: "200"}); err == nil {
		t.Fatal("missing comment properties accepted")
	}
}

func TestJiraCopyCarriesIdentityProperty(t *testing.T) {
	t.Parallel()
	srv := newServer(t, ok(Object{"id": "300"}))
	op := strings.Repeat("d", 64)
	if _, err := srv.client(t).CreateComment(SideJira, Pair{JiraID: "200"}, "hello", op); err != nil {
		t.Fatal(err)
	}
	props := list(srv.bodies[0]["properties"])
	eq(t, string(canonical(props, false)), `[{"key":"gh-jira-sync","value":{"operation":"`+op+`"}}]`)
	text, _ := adfText(srv.bodies[0]["body"])
	eq(t, text, "hello")
}

func TestFindIssueSearchesByCreatorSinceIntent(t *testing.T) {
	t.Parallel()
	var jql []string
	api := (*Client)(nil)
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		if strings.HasPrefix(r.URL.Path, "/rest/api/3/search/jql") {
			jql = append(jql, r.URL.Query().Get("jql"))
			op := token(api.c, OpIssue, "42")
			return 200, nil, Object{"issues": []any{
				Object{"id": "199", "properties": Object{"gh-jira-sync": Object{"operation": op, "github_id": "41"}}},
				Object{"id": "200", "properties": Object{"gh-jira-sync": Object{"operation": op, "github_id": "42"}}},
				Object{"id": "201"},
			}}
		}
		op := token(api.c, OpIssue, "42")
		return 200, nil, Object{
			"id": "200", "key": "ENG-7", "fields": Object{"project": Object{"key": "ENG"}},
			"properties": Object{"gh-jira-sync": Object{"operation": op, "github_id": "42"}},
		}
	})
	api = srv.client(t)
	source := newFake().source
	started := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)
	issue, err := api.FindIssue(&source, started)
	if err != nil || issue == nil || issue.Key != "ENG-7" {
		t.Fatalf("got %+v %v", issue, err)
	}
	if !strings.Contains(jql[0], "creator = currentUser()") || !strings.Contains(jql[0], "created >= -40m") || strings.Contains(jql[0], "labels") {
		t.Fatalf("jql: %s", jql[0])
	}
	// Intents journaled by older releases have no start time and use their label.
	if _, err := api.FindIssue(&source, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jql[1], `labels = "gh-sync-`) {
		t.Fatalf("legacy jql: %s", jql[1])
	}
}

func TestJiraCreationHasAtomicIdentity(t *testing.T) {
	t.Parallel()
	srv := newServer(t, ok(Object{"id": "200", "key": "ENG-1"}))
	api := srv.client(t)
	source := newFake().source
	if _, err := api.CreateIssue(&source, ""); err != nil {
		t.Fatal(err)
	}
	payload := srv.bodies[0]
	prop := asObject(asObject(list(payload["properties"])[0])["value"])
	eq(t, prop["github_id"], any("42"))
	eq(t, prop["operation"], any(token(api.c, OpIssue, "42")))
	fields := asObject(payload["fields"])
	if _, labeled := fields["labels"]; labeled {
		t.Fatal("identity must live only in the issue property, not a visible label")
	}
	eq(t, fields["summary"], any("Original title"))
	// The description is rich text: the source link is a link, the body follows.
	url := "https://github.com/acme/test/issues/1"
	eq(t, string(canonical(fields["description"], false)), string(canonical(Object{"type": "doc", "version": 1, "content": []any{
		Object{"type": "paragraph", "content": []any{
			Object{"type": "text", "text": "From GitHub: "},
			Object{"type": "text", "text": url, "marks": []any{Object{"type": "link", "attrs": Object{"href": url}}}},
		}},
		Object{"type": "paragraph", "content": []any{Object{"type": "text", "text": "description"}}},
	}}, false)))
}

func TestServiceDeskProjectRejected(t *testing.T) {
	t.Parallel()
	srv := newServer(t, ok(Object{"projectTypeKey": "service_desk"}))
	if srv.client(t).CheckProject() == nil {
		t.Fatal("service desk project accepted")
	}
}

func TestPlainADFRoundTrip(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"", "hello\nworld\n", "é 👋", "# **literal**", "\n\n"} {
		got, err := adfText(decodeJSON(canonical(toADF(text), false)))
		if err != nil || got != text {
			t.Errorf("round trip %q = %q, %v", text, got, err)
		}
	}
}

func TestGoneGitHubIssueReturnsNil(t *testing.T) {
	t.Parallel()
	for _, code := range []int{301, 404, 410} {
		srv := newServer(t, status(code, http.Header{"Location": {"https://elsewhere.test/"}}))
		issue, err := srv.client(t).Issue(1)
		if err != nil || issue != nil {
			t.Errorf("HTTP %d: issue=%v err=%v", code, issue, err)
		}
	}
	srv := newServer(t, status(403, nil))
	if _, err := srv.client(t).Issue(1); err == nil {
		t.Fatal("403 must be an error")
	}
	eq(t, len(srv.urls), 1)
}

func TestGoneOrMovedJiraIssueReturnsNil(t *testing.T) {
	t.Parallel()
	if issue, err := newServer(t, status(404, nil)).client(t).JiraIssue("200"); err != nil || issue != nil {
		t.Fatalf("404: %v %v", issue, err)
	}
	moved := newServer(t, ok(Object{"id": "200", "fields": Object{"project": Object{"key": "OTHER"}}}))
	if issue, err := moved.client(t).JiraIssue("200"); err != nil || issue != nil {
		t.Fatalf("moved: %v %v", issue, err)
	}
	if _, err := newServer(t, status(401, nil)).client(t).JiraIssue("200"); err == nil {
		t.Fatal("401 must be an error")
	}
}

func TestHTTPErrorCarriesStatus(t *testing.T) {
	t.Parallel()
	srv := newServer(t, status(301, http.Header{"Location": {"https://elsewhere.test/"}}))
	err := srv.http().Do("GET", "/x", nil, nil)
	code, isHTTP := httpStatus(err)
	if !isHTTP || code != 301 {
		t.Fatalf("got %v", err)
	}
	eq(t, len(srv.urls), 1)
}

func TestGitHubRateLimit403RetriedForReadsOnly(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := newServer(t, func(*http.Request) (int, http.Header, any) {
		calls++
		if calls == 1 {
			return 403, http.Header{"Retry-After": {"7"}}, nil
		}
		return 200, nil, Object{"ok": true}
	})
	var got Object
	err := srv.http().Do("GET", "/x", nil, &got)
	if err != nil || !sameJSON(got, Object{"ok": true}) {
		t.Fatalf("got %v %v", got, err)
	}
	eq(t, srv.sleeps, []time.Duration{7 * time.Second})

	limited := newServer(t, status(403, http.Header{"Retry-After": {"7"}}))
	if err := limited.http().Do("POST", "/x", Object{}, nil); err == nil {
		t.Fatal("rate-limited write must not be retried")
	}
	eq(t, len(limited.urls), 1)

	plain := newServer(t, status(403, nil))
	if err := plain.http().Do("GET", "/x", nil, nil); err == nil {
		t.Fatal("plain 403 must fail")
	}
	eq(t, len(plain.urls), 1)
}

func TestRateLimitResetDelayIsCapped(t *testing.T) {
	t.Parallel()
	h := newHTTP("https://example.test", nil)
	h.now = func() time.Time { return time.Unix(1000, 0) }
	eq(t, h.retryDelay(http.Header{"X-Ratelimit-Reset": {"1005"}}, 0), 5*time.Second)
	eq(t, h.retryDelay(http.Header{"X-Ratelimit-Reset": {"9999"}}, 0), 60*time.Second)
	eq(t, h.retryDelay(http.Header{}, 2), 4*time.Second)
}

func TestInvalidJSONResponseIsReported(t *testing.T) {
	t.Parallel()
	srv := newServer(t, ok(nil))
	for _, body := range []string{`{"a":1} trailing`, `{"a":`} {
		srv.respond = func(*http.Request) (int, http.Header, any) { return 200, nil, []byte(body) }
		var v Object
		if err := srv.http().Do("GET", "/x", nil, &v); err == nil {
			t.Fatalf("invalid JSON %q accepted", body)
		}
	}
}

func TestWrongAPIShapesFailLoudly(t *testing.T) {
	t.Parallel()
	// A string where GitHub promises a number must fail, not read as issue 0.
	issue := newServer(t, ok(Object{"id": 42, "number": "1", "title": "x"}))
	if _, err := issue.client(t).Issue(1); err == nil {
		t.Fatal("mistyped issue number accepted")
	}
	comments := newServer(t, ok(Object{"comments": []any{Object{"id": 5}}, "total": 1}))
	if _, err := comments.client(t).Comments(SideJira, Pair{JiraID: "200"}); err == nil {
		t.Fatal("numeric Jira comment id accepted")
	}
	project := newServer(t, ok([]any{}))
	if err := project.client(t).CheckProject(); err == nil {
		t.Fatal("array project response accepted")
	}
}

func decodeComment[T interface{ comment() Comment }](t *testing.T, raw string) Comment {
	t.Helper()
	var wire T
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&wire); err != nil {
		t.Fatal(err)
	}
	return wire.comment()
}

func TestCommentIgnoresFieldsThatDoNotDriveDecisions(t *testing.T) {
	t.Parallel()
	before := decodeComment[apiGitHubComment](t, `{"id":11,"body":"hi","created_at":"2026-01-01T00:00:00Z",
		"updated_at":"2026-01-01T00:00:00Z","user":{"login":"alice","id":7},"reactions":{"total_count":0}}`)
	after := decodeComment[apiGitHubComment](t, `{"id":11,"body":"hi","created_at":"2026-01-01T00:00:00Z",
		"updated_at":"2026-02-01T00:00:00Z","user":{"login":"alice","id":7},"reactions":{"total_count":3}}`)
	if !before.sameAs(after) {
		t.Fatal("reactions or updated_at changed the decision view")
	}
	edited := decodeComment[apiGitHubComment](t, `{"id":11,"body":"edited","created_at":"2026-01-01T00:00:00Z"}`)
	if before.sameAs(edited) {
		t.Fatal("body edit not detected")
	}
	ghost := decodeComment[apiGitHubComment](t, `{"id":12,"body":"x","created_at":"2026-01-01T00:00:00Z","user":null}`)
	eq(t, ghost.Author, "")
}

func TestJiraRestrictionSignals(t *testing.T) {
	t.Parallel()
	adf := `"body":{"type":"doc","version":1,"content":[]}`
	for raw, want := range map[string]bool{
		`{"id":"1",` + adf + `}`:                                                                      false,
		`{"id":"1",` + adf + `,"visibility":{}}`:                                                      false,
		`{"id":"1",` + adf + `,"visibility":{"type":"role","value":"Developers"}}`:                    true,
		`{"id":"1",` + adf + `,"jsdPublic":false}`:                                                    true,
		`{"id":"1",` + adf + `,"jsdPublic":true}`:                                                     false,
		`{"id":"1",` + adf + `,"properties":[{"key":"sd.public.comment","value":{"internal":true}}]}`: true,
		`{"id":"1",` + adf + `,"properties":[{"key":"other","value":"any shape"}]}`:                   false,
	} {
		if got := decodeComment[apiJiraComment](t, raw).Restricted; got != want {
			t.Errorf("%s: restricted=%v, want %v", raw, got, want)
		}
	}
}
