package ghjira

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// API is everything planning and applying need from GitHub and Jira.
type API interface {
	CheckProject() error
	Issues() ([]Object, error)
	// Issue returns nil when the issue was deleted or transferred.
	Issue(number int) (Object, error)
	FindIssue(source Object) (Object, error)
	// JiraIssue returns nil when the issue was deleted, hidden, or moved out of the project.
	JiraIssue(id string) (Object, error)
	CreateIssue(source Object) (Object, error)
	Comments(side string, pair Pair) ([]Object, error)
	Comment(side string, pair Pair, id string) (Object, error)
	CreateComment(side string, pair Pair, text string) (Object, error)
	EditComment(side string, pair Pair, id, text string) error
}

// HTTP is a small JSON REST client. Only GET requests are automatically retried.
type HTTP struct {
	base    string
	headers map[string]string
	client  *http.Client
	sleep   func(time.Duration)
	now     func() time.Time
}

func newHTTP(base string, headers map[string]string) *HTTP {
	return &HTTP{
		base:    base,
		headers: headers,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// Credentials never follow redirects; a 3xx surfaces as an HTTPStatusError.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sleep: time.Sleep,
		now:   time.Now,
	}
}

// retryDelay honors Retry-After or x-ratelimit-reset, capped at a minute; otherwise backs off.
func (h *HTTP) retryDelay(headers http.Header, attempt int) time.Duration {
	if n, err := strconv.Atoi(headers.Get("Retry-After")); err == nil && n >= 0 {
		return time.Duration(min(60, n)) * time.Second
	}
	if reset, err := strconv.ParseInt(headers.Get("x-ratelimit-reset"), 10, 64); err == nil && reset >= 0 {
		return time.Duration(min(60, max(1, reset-h.now().Unix()))) * time.Second
	}
	return time.Duration(math.Pow(2, float64(attempt))) * time.Second
}

// Request sends JSON and decodes a JSON response (nil for an empty body).
func (h *HTTP) Request(method, path string, body any) (any, error) {
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			return nil, syncErr("Invalid request body")
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(method, h.base+path, bytes.NewReader(data))
		if err != nil {
			return nil, syncErr("%s network failure; write outcome may be unknown", method)
		}
		for k, v := range h.headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.client.Do(req)
		var result []byte
		if err == nil {
			result, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		if err != nil {
			if method == http.MethodGet && attempt < 3 {
				h.sleep(time.Duration(math.Pow(2, float64(attempt))) * time.Second)
				continue
			}
			return nil, syncErr("%s network failure; write outcome may be unknown", method)
		}
		if code := resp.StatusCode; code >= 300 {
			// GitHub signals primary and secondary rate limits with 403 plus these headers.
			limited := code == 403 && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("x-ratelimit-remaining") == "0")
			transient := code == 429 || code == 500 || code == 502 || code == 503 || code == 504
			if method == http.MethodGet && (transient || limited) && attempt < 3 {
				h.sleep(h.retryDelay(resp.Header, attempt))
				continue
			}
			// API response bodies can contain issue text or credentials; never log them.
			return nil, &HTTPStatusError{Status: code, Msg: method + " failed with HTTP " + strconv.Itoa(code) + "; writes are not retried"}
		}
		if len(result) == 0 {
			return nil, nil
		}
		dec := json.NewDecoder(bytes.NewReader(result))
		dec.UseNumber()
		var out any
		if dec.Decode(&out) != nil || dec.Decode(new(any)) != io.EOF {
			return nil, syncErr("Invalid API response; write outcome may be unknown")
		}
		return out, nil
	}
}

func (h *HTTP) object(method, path string, body any) (Object, error) {
	v, err := h.Request(method, path, body)
	if err != nil {
		return nil, err
	}
	o, ok := v.(Object)
	if !ok {
		return nil, syncErr("Invalid API response; write outcome may be unknown")
	}
	return o, nil
}

// Client talks to GitHub and Jira Cloud with narrowly scoped requests.
type Client struct {
	c      *Config
	github *HTTP
	jira   *HTTP
}

// NewClient reads credentials from GH_TOKEN, JIRA_EMAIL, and JIRA_API_TOKEN.
func NewClient(c *Config) (*Client, error) {
	gh, email, apiToken := os.Getenv("GH_TOKEN"), os.Getenv("JIRA_EMAIL"), os.Getenv("JIRA_API_TOKEN")
	if gh == "" || email == "" || apiToken == "" {
		return nil, syncErr("Set GH_TOKEN, JIRA_EMAIL, and JIRA_API_TOKEN")
	}
	basic := base64.StdEncoding.EncodeToString([]byte(email + ":" + apiToken))
	return &Client{
		c: c,
		github: newHTTP(c.GitHubURL, map[string]string{"Authorization": "Bearer " + gh,
			"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}),
		jira: newHTTP(c.JiraURL, map[string]string{"Authorization": "Basic " + basic, "Accept": "application/json"}),
	}, nil
}

func (a *Client) CheckProject() error {
	project, err := a.jira.object("GET", "/rest/api/3/project/"+a.c.JiraProject, nil)
	if err != nil {
		return err
	}
	if t := project["projectTypeKey"]; t != "software" && t != "business" {
		return syncErr("Only Jira Software/business projects supported; service-desk privacy requires separate handling")
	}
	return nil
}

func (a *Client) ghPath(number int) string {
	base := "/repos/" + a.c.Repository + "/issues"
	if number == 0 {
		return base
	}
	return base + "/" + strconv.Itoa(number)
}

func (a *Client) ghPages(path string, params url.Values) ([]Object, error) {
	var result []Object
	for page := 1; page <= a.c.MaxPages; page++ {
		q := url.Values{}
		maps.Copy(q, params)
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		v, err := a.github.Request("GET", path+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		batch, ok := v.([]any)
		if !ok {
			return nil, syncErr("Invalid GitHub pagination response")
		}
		for _, item := range batch {
			o, ok := item.(Object)
			if !ok {
				return nil, syncErr("Invalid GitHub pagination response")
			}
			result = append(result, o)
		}
		if len(batch) < 100 {
			return result, nil
		}
	}
	return nil, syncErr("GitHub pagination limit exceeded; no partial scan allowed")
}

func (a *Client) Issues() ([]Object, error) {
	params := url.Values{"state": {a.c.IssueState}, "sort": {"created"}, "direction": {"asc"}}
	if a.c.SelectionLabel != nil {
		params.Set("labels", *a.c.SelectionLabel)
	}
	all, err := a.ghPages(a.ghPath(0), params)
	if err != nil {
		return nil, err
	}
	var issues []Object
	for _, i := range all {
		if _, pr := i["pull_request"]; !pr {
			issues = append(issues, i)
		}
	}
	return issues, nil
}

func (a *Client) Issue(number int) (Object, error) {
	issue, err := a.github.object("GET", a.ghPath(number), nil)
	if status, ok := httpStatus(err); ok && (status == 301 || status == 404 || status == 410) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, pr := issue["pull_request"]; pr || idString(issue["number"]) != strconv.Itoa(number) {
		return nil, syncErr("Issue identity mismatch or pull request")
	}
	return issue, nil
}

func httpStatus(err error) (int, bool) {
	h, ok := err.(*HTTPStatusError)
	if !ok {
		return 0, false
	}
	return h.Status, true
}

func (a *Client) JiraIssue(id string) (Object, error) {
	result, err := a.jira.object("GET", "/rest/api/3/issue/"+id+"?fields=project,labels,updated&properties=gh-jira-sync", nil)
	if status, ok := httpStatus(err); ok && status == 404 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if asObject(asObject(result["fields"])["project"])["key"] != a.c.JiraProject {
		return nil, nil
	}
	return result, nil
}

func (a *Client) FindIssue(source Object) (Object, error) {
	op := token(a.c, "issue", source["id"])
	query := url.Values{"jql": {`project = "` + a.c.JiraProject + `" AND labels = "gh-sync-` + op + `"`},
		"maxResults": {"2"}, "fields": {"id,key"}}
	result, err := a.jira.object("GET", "/rest/api/3/search/jql?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	found := list(result["issues"])
	if truthy(result["nextPageToken"]) || len(found) > 1 {
		return nil, syncErr("Multiple Jira issues have the same source identity")
	}
	if len(found) == 0 {
		return nil, nil
	}
	issue, err := a.JiraIssue(idString(asObject(found[0])["id"]))
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, syncErr("Jira search result vanished or moved; rerun")
	}
	prop := asObject(issue["properties"])["gh-jira-sync"]
	if !sameJSON(prop, Object{"operation": op, "github_id": idString(source["id"])}) {
		return nil, syncErr("Jira source identity marker mismatch")
	}
	return issue, nil
}

func (a *Client) CreateIssue(source Object) (Object, error) {
	op := token(a.c, "issue", source["id"])
	fields := Object{}
	maps.Copy(fields, a.c.JiraCreateFields)
	fields["project"] = Object{"key": a.c.JiraProject}
	fields["issuetype"] = Object{"id": a.c.JiraIssueType}
	fields["summary"] = source["title"]
	fields["description"] = toADF("Source: " + idString(source["html_url"]) + "\n\n" + normalized(source["body"]))
	fields["labels"] = []any{"gh-sync-" + op}
	return a.jira.object("POST", "/rest/api/3/issue", Object{"fields": fields, "properties": []any{
		Object{"key": "gh-jira-sync", "value": Object{"operation": op, "github_id": idString(source["id"])}}}})
}

func (a *Client) Comments(side string, pair Pair) ([]Object, error) {
	if side == "github" {
		return a.ghPages(a.ghPath(pair.GitHub)+"/comments", nil)
	}
	var result []Object
	start := 0
	for range a.c.MaxPages {
		q := url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {"100"}, "orderBy": {"created"}, "expand": {"properties"}}
		batch, err := a.jira.object("GET", "/rest/api/3/issue/"+pair.JiraID+"/comment?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		items := list(batch["comments"])
		for _, item := range items {
			o, ok := item.(Object)
			if !ok {
				return nil, syncErr("Invalid API response; write outcome may be unknown")
			}
			result = append(result, o)
		}
		start += len(items)
		total, err := strconv.Atoi(idString(batch["total"]))
		if err != nil {
			return nil, syncErr("Invalid API response; write outcome may be unknown")
		}
		if start >= total {
			return result, nil
		}
		if len(items) == 0 {
			return nil, syncErr("Incomplete Jira comment pagination")
		}
	}
	return nil, syncErr("Jira pagination limit exceeded; no partial scan allowed")
}

func (a *Client) Comment(side string, pair Pair, id string) (Object, error) {
	if side == "github" {
		return a.github.object("GET", "/repos/"+a.c.Repository+"/issues/comments/"+id, nil)
	}
	return a.jira.object("GET", "/rest/api/3/issue/"+pair.JiraID+"/comment/"+id+"?expand=properties", nil)
}

func (a *Client) CreateComment(side string, pair Pair, text string) (Object, error) {
	if side == "github" {
		return a.github.object("POST", a.ghPath(pair.GitHub)+"/comments", Object{"body": text})
	}
	return a.jira.object("POST", "/rest/api/3/issue/"+pair.JiraID+"/comment", Object{"body": toADF(text)})
}

func (a *Client) EditComment(side string, pair Pair, id, text string) error {
	var err error
	if side == "github" {
		_, err = a.github.Request("PATCH", "/repos/"+a.c.Repository+"/issues/comments/"+id, Object{"body": text})
	} else {
		_, err = a.jira.Request("PUT", "/rest/api/3/issue/"+pair.JiraID+"/comment/"+id, Object{"body": toADF(text)})
	}
	return err
}
