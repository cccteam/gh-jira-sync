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
			w.Write(raw)
		} else if payload != nil {
			json.NewEncoder(w).Encode(payload)
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
	srv := newServer(t, ok(nil))
	h := srv.http()
	srv.instance.Close() // every request now fails at the network level
	if _, err := h.Request("POST", "/issue", Object{}); err == nil {
		t.Fatal("expected network failure")
	}
	eq(t, len(srv.sleeps), 0)
}

func TestReadRetries(t *testing.T) {
	srv := newServer(t, ok(nil))
	h := srv.http()
	srv.instance.Close()
	if _, err := h.Request("GET", "/issue", nil); err == nil {
		t.Fatal("expected network failure")
	}
	eq(t, srv.sleeps, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second})
}

func TestIssueSelectionAndPRFilter(t *testing.T) {
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
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		if r.URL.Query().Get("page") == "1" {
			return 200, nil, page(100, 0)
		}
		return 200, nil, page(1, 100)
	})
	api := srv.client(t)
	rows, err := api.ghPages("/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(rows), 101)
	api.c.MaxPages = 1
	if _, err := api.ghPages("/test", nil); err == nil {
		t.Fatal("partial scan must fail")
	}
}

func TestJiraPagination(t *testing.T) {
	srv := newServer(t, func(r *http.Request) (int, http.Header, any) {
		start := r.URL.Query().Get("startAt")
		return 200, nil, Object{"comments": []any{Object{"id": "c" + start}}, "total": 2}
	})
	rows, err := srv.client(t).Comments("jira", Pair{JiraID: "200"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(rows), 2)
	if !strings.Contains(srv.urls[1], "startAt=1") {
		t.Fatalf("second page not requested from offset 1: %s", srv.urls[1])
	}
}

func TestJiraCreationHasAtomicIdentity(t *testing.T) {
	srv := newServer(t, ok(Object{"id": "200", "key": "ENG-1"}))
	api := srv.client(t)
	if _, err := api.CreateIssue(cp(newFake().source)); err != nil {
		t.Fatal(err)
	}
	payload := srv.bodies[0]
	prop := asObject(asObject(list(payload["properties"])[0])["value"])
	eq(t, prop["github_id"], any("42"))
	fields := asObject(payload["fields"])
	eq(t, list(fields["labels"]), []any{"gh-sync-" + token(api.c, "issue", 42)})
	eq(t, fields["summary"], any("Original title"))
}

func TestServiceDeskProjectRejected(t *testing.T) {
	srv := newServer(t, ok(Object{"projectTypeKey": "service_desk"}))
	if srv.client(t).CheckProject() == nil {
		t.Fatal("service desk project accepted")
	}
}

func TestPlainADFRoundTrip(t *testing.T) {
	for _, text := range []string{"", "hello\nworld\n", "é 👋", "# **literal**", "\n\n"} {
		got, err := adfText(decodeJSON(canonical(toADF(text), false)))
		if err != nil || got != text {
			t.Errorf("round trip %q = %q, %v", text, got, err)
		}
	}
}

func TestGoneGitHubIssueReturnsNil(t *testing.T) {
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
	srv := newServer(t, status(301, http.Header{"Location": {"https://elsewhere.test/"}}))
	_, err := srv.http().Request("GET", "/x", nil)
	code, isHTTP := httpStatus(err)
	if !isHTTP || code != 301 {
		t.Fatalf("got %v", err)
	}
	eq(t, len(srv.urls), 1)
}

func TestGitHubRateLimit403RetriedForReadsOnly(t *testing.T) {
	calls := 0
	srv := newServer(t, func(*http.Request) (int, http.Header, any) {
		calls++
		if calls == 1 {
			return 403, http.Header{"Retry-After": {"7"}}, nil
		}
		return 200, nil, Object{"ok": true}
	})
	got, err := srv.http().Request("GET", "/x", nil)
	if err != nil || !sameJSON(got, Object{"ok": true}) {
		t.Fatalf("got %v %v", got, err)
	}
	eq(t, srv.sleeps, []time.Duration{7 * time.Second})

	limited := newServer(t, status(403, http.Header{"Retry-After": {"7"}}))
	if _, err := limited.http().Request("POST", "/x", Object{}); err == nil {
		t.Fatal("rate-limited write must not be retried")
	}
	eq(t, len(limited.urls), 1)

	plain := newServer(t, status(403, nil))
	if _, err := plain.http().Request("GET", "/x", nil); err == nil {
		t.Fatal("plain 403 must fail")
	}
	eq(t, len(plain.urls), 1)
}

func TestRateLimitResetDelayIsCapped(t *testing.T) {
	h := newHTTP("https://example.test", nil)
	h.now = func() time.Time { return time.Unix(1000, 0) }
	eq(t, h.retryDelay(http.Header{"X-Ratelimit-Reset": {"1005"}}, 0), 5*time.Second)
	eq(t, h.retryDelay(http.Header{"X-Ratelimit-Reset": {"9999"}}, 0), 60*time.Second)
	eq(t, h.retryDelay(http.Header{}, 2), 4*time.Second)
}

func TestInvalidJSONResponseIsReported(t *testing.T) {
	srv := newServer(t, ok(nil))
	for _, body := range []string{`{"a":1} trailing`, `{"a":`} {
		srv.respond = func(*http.Request) (int, http.Header, any) { return 200, nil, []byte(body) }
		if _, err := srv.http().Request("GET", "/x", nil); err == nil {
			t.Fatalf("invalid JSON %q accepted", body)
		}
	}
}
