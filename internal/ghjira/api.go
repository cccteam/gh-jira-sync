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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// API is everything planning and applying need from GitHub and Jira.
type API interface {
	CheckProject() error
	Issues() ([]GitHubIssue, error)
	// Issue returns nil when the issue was deleted or transferred.
	Issue(number int) (*GitHubIssue, error)
	FindIssue(source *GitHubIssue) (*JiraIssue, error)
	// JiraIssue returns nil when the issue was deleted, hidden, or moved out of the project.
	JiraIssue(id string) (*JiraIssue, error)
	CreateIssue(source *GitHubIssue) (*JiraIssue, error)
	Comments(side Side, pair Pair) ([]Comment, error)
	Comment(side Side, pair Pair, id string) (Comment, error)
	CreateComment(side Side, pair Pair, text string) (Comment, error)
	EditComment(side Side, pair Pair, id, text string) error
	// JiraTransitions lists the workflow moves available from the issue's current status.
	JiraTransitions(id string) ([]JiraTransition, error)
	TransitionJira(id, transitionID string) error
	SetGitHubState(number int, closed bool) error
	// Project reads the configured project's status field and every issue item's value.
	Project() (*ProjectSnapshot, error)
	ProjectItem(itemID, fieldID int64) (ProjectItem, error)
	AddProjectItem(number int) error
	SetProjectStatus(itemID, fieldID int64, optionID string) error
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

// Do sends body as JSON and decodes the response into out (unless out is nil).
// A response that does not fit out's type is an error, never silently zero values.
func (h *HTTP) Do(method, path string, body, out any) error {
	_, err := h.exchange(method, path, body, out)
	return err
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// DoPage is Do for cursor-paginated GET requests: it also returns the path of the
// next page from the Link header, or "" on the last page. The next page must be on
// the same host, so credentials are never sent elsewhere.
func (h *HTTP) DoPage(path string, out any) (string, error) {
	headers, err := h.exchange(http.MethodGet, path, nil, out)
	if err != nil {
		return "", err
	}
	m := nextLink.FindStringSubmatch(headers.Get("Link"))
	if m == nil {
		return "", nil
	}
	if !strings.HasPrefix(m[1], h.base+"/") {
		return "", syncErr("Unexpected pagination link; refusing to follow it")
	}
	return strings.TrimPrefix(m[1], h.base), nil
}

func (h *HTTP) exchange(method, path string, body, out any) (http.Header, error) {
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
		if out == nil {
			return resp.Header, nil
		}
		dec := json.NewDecoder(bytes.NewReader(result))
		dec.UseNumber() // raw ADF bodies keep exact numbers for hashing
		if dec.Decode(out) != nil || dec.Decode(new(json.RawMessage)) != io.EOF {
			return nil, syncErr("Invalid API response; write outcome may be unknown")
		}
		return resp.Header, nil
	}
}

// Client talks to GitHub and Jira Cloud with narrowly scoped requests.
type Client struct {
	c        *Config
	github   *HTTP
	jira     *HTTP
	projects *HTTP // project status mode only; uses GH_PROJECT_TOKEN
}

// NewClient reads credentials from GH_TOKEN, JIRA_EMAIL, and JIRA_API_TOKEN, plus
// GH_PROJECT_TOKEN in project status mode.
func NewClient(c *Config) (*Client, error) {
	gh, email, apiToken := os.Getenv("GH_TOKEN"), os.Getenv("JIRA_EMAIL"), os.Getenv("JIRA_API_TOKEN")
	if gh == "" || email == "" || apiToken == "" {
		return nil, syncErr("Set GH_TOKEN, JIRA_EMAIL, and JIRA_API_TOKEN")
	}
	basic := base64.StdEncoding.EncodeToString([]byte(email + ":" + apiToken))
	client := &Client{
		c: c,
		github: newHTTP(c.GitHubURL, map[string]string{"Authorization": "Bearer " + gh,
			"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}),
		jira: newHTTP(c.JiraURL, map[string]string{"Authorization": "Basic " + basic, "Accept": "application/json"}),
	}
	if c.ProjectStatus != nil {
		projectToken := os.Getenv("GH_PROJECT_TOKEN")
		if projectToken == "" {
			return nil, syncErr("Set GH_PROJECT_TOKEN for project_status; GitHub Actions tokens cannot reach organization projects")
		}
		// The Projects REST endpoints are documented under this newer API version.
		client.projects = newHTTP(c.GitHubURL, map[string]string{"Authorization": "Bearer " + projectToken,
			"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2026-03-10"})
	}
	return client, nil
}

func (a *Client) CheckProject() error {
	var project struct {
		ProjectTypeKey string `json:"projectTypeKey"`
	}
	if err := a.jira.Do("GET", "/rest/api/3/project/"+a.c.JiraProject, nil, &project); err != nil {
		return err
	}
	if project.ProjectTypeKey != "software" && project.ProjectTypeKey != "business" {
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

// ghPages reads every page or fails; a partial scan could look like deletions.
func ghPages[T any](a *Client, path string, params url.Values) ([]T, error) {
	var result []T
	for page := 1; page <= a.c.MaxPages; page++ {
		q := url.Values{}
		maps.Copy(q, params)
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		var batch []T
		if err := a.github.Do("GET", path+"?"+q.Encode(), nil, &batch); err != nil {
			return nil, err
		}
		result = append(result, batch...)
		if len(batch) < 100 {
			return result, nil
		}
	}
	return nil, syncErr("GitHub pagination limit exceeded; no partial scan allowed")
}

func (a *Client) Issues() ([]GitHubIssue, error) {
	params := url.Values{"state": {a.c.IssueState}, "sort": {"created"}, "direction": {"asc"}}
	if a.c.SelectionLabel != nil {
		params.Set("labels", *a.c.SelectionLabel)
	}
	all, err := ghPages[GitHubIssue](a, a.ghPath(0), params)
	if err != nil {
		return nil, err
	}
	var issues []GitHubIssue
	for _, i := range all {
		if i.PullRequest == nil {
			issues = append(issues, i)
		}
	}
	return issues, nil
}

func (a *Client) Issue(number int) (*GitHubIssue, error) {
	issue := &GitHubIssue{}
	err := a.github.Do("GET", a.ghPath(number), nil, issue)
	if status, ok := httpStatus(err); ok && (status == 301 || status == 404 || status == 410) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if issue.PullRequest != nil || issue.Number != number {
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

func (a *Client) JiraIssue(id string) (*JiraIssue, error) {
	issue := &JiraIssue{}
	err := a.jira.Do("GET", "/rest/api/3/issue/"+id+"?fields=project,labels,updated,status&properties=gh-jira-sync", nil, issue)
	if status, ok := httpStatus(err); ok && status == 404 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if issue.Fields.Project.Key != a.c.JiraProject {
		return nil, nil
	}
	return issue, nil
}

func (a *Client) FindIssue(source *GitHubIssue) (*JiraIssue, error) {
	op := token(a.c, OpIssue, source.idString())
	query := url.Values{"jql": {`project = "` + a.c.JiraProject + `" AND labels = "gh-sync-` + op + `"`},
		"maxResults": {"2"}, "fields": {"id,key"}}
	var result struct {
		Issues []struct {
			ID string `json:"id"`
		} `json:"issues"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := a.jira.Do("GET", "/rest/api/3/search/jql?"+query.Encode(), nil, &result); err != nil {
		return nil, err
	}
	if result.NextPageToken != "" || len(result.Issues) > 1 {
		return nil, syncErr("Multiple Jira issues have the same source identity")
	}
	if len(result.Issues) == 0 {
		return nil, nil
	}
	issue, err := a.JiraIssue(result.Issues[0].ID)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, syncErr("Jira search result vanished or moved; rerun")
	}
	if id := issue.Properties.Identity; id == nil || *id != (IssueIdentity{Operation: op, GitHubID: source.idString()}) {
		return nil, syncErr("Jira source identity marker mismatch")
	}
	return issue, nil
}

func (a *Client) CreateIssue(source *GitHubIssue) (*JiraIssue, error) {
	op := token(a.c, OpIssue, source.idString())
	// Extra fields are tenant-specific Jira fields, so they stay generic JSON.
	fields := Object{}
	maps.Copy(fields, a.c.JiraCreateFields)
	fields["project"] = Object{"key": a.c.JiraProject}
	fields["issuetype"] = Object{"id": a.c.JiraIssueType}
	fields["summary"] = source.Title
	fields["description"] = toADF("Source: " + source.HTMLURL + "\n\n" + normalized(source.Body))
	fields["labels"] = []string{"gh-sync-" + op}
	request := struct {
		Fields     Object `json:"fields"`
		Properties []any  `json:"properties"`
	}{fields, []any{struct {
		Key   string        `json:"key"`
		Value IssueIdentity `json:"value"`
	}{"gh-jira-sync", IssueIdentity{Operation: op, GitHubID: source.idString()}}}}
	created := &JiraIssue{}
	if err := a.jira.Do("POST", "/rest/api/3/issue", request, created); err != nil {
		return nil, err
	}
	return created, nil
}

func (a *Client) Comments(side Side, pair Pair) ([]Comment, error) {
	if side == SideGitHub {
		raw, err := ghPages[apiGitHubComment](a, a.ghPath(pair.GitHub)+"/comments", nil)
		if err != nil {
			return nil, err
		}
		result := make([]Comment, len(raw))
		for i, g := range raw {
			result[i] = g.comment()
		}
		return result, nil
	}
	var result []Comment
	start := 0
	for range a.c.MaxPages {
		q := url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {"100"}, "orderBy": {"created"}, "expand": {"properties"}}
		var batch struct {
			Comments []apiJiraComment `json:"comments"`
			Total    int              `json:"total"`
		}
		if err := a.jira.Do("GET", "/rest/api/3/issue/"+pair.JiraID+"/comment?"+q.Encode(), nil, &batch); err != nil {
			return nil, err
		}
		for _, j := range batch.Comments {
			result = append(result, j.comment())
		}
		start += len(batch.Comments)
		if start >= batch.Total {
			return result, nil
		}
		if len(batch.Comments) == 0 {
			return nil, syncErr("Incomplete Jira comment pagination")
		}
	}
	return nil, syncErr("Jira pagination limit exceeded; no partial scan allowed")
}

func (a *Client) Comment(side Side, pair Pair, id string) (Comment, error) {
	if side == SideGitHub {
		var g apiGitHubComment
		err := a.github.Do("GET", "/repos/"+a.c.Repository+"/issues/comments/"+id, nil, &g)
		return g.comment(), err
	}
	var j apiJiraComment
	err := a.jira.Do("GET", "/rest/api/3/issue/"+pair.JiraID+"/comment/"+id+"?expand=properties", nil, &j)
	return j.comment(), err
}

func (a *Client) CreateComment(side Side, pair Pair, text string) (Comment, error) {
	if side == SideGitHub {
		var g apiGitHubComment
		err := a.github.Do("POST", a.ghPath(pair.GitHub)+"/comments", map[string]string{"body": text}, &g)
		return g.comment(), err
	}
	var j apiJiraComment
	err := a.jira.Do("POST", "/rest/api/3/issue/"+pair.JiraID+"/comment", map[string]any{"body": toADF(text)}, &j)
	return j.comment(), err
}

func (a *Client) EditComment(side Side, pair Pair, id, text string) error {
	if side == SideGitHub {
		return a.github.Do("PATCH", "/repos/"+a.c.Repository+"/issues/comments/"+id, map[string]string{"body": text}, nil)
	}
	return a.jira.Do("PUT", "/rest/api/3/issue/"+pair.JiraID+"/comment/"+id, map[string]any{"body": toADF(text)}, nil)
}

func (a *Client) JiraTransitions(id string) ([]JiraTransition, error) {
	var result struct {
		Transitions []JiraTransition `json:"transitions"`
	}
	if err := a.jira.Do("GET", "/rest/api/3/issue/"+id+"/transitions", nil, &result); err != nil {
		return nil, err
	}
	return result.Transitions, nil
}

func (a *Client) TransitionJira(id, transitionID string) error {
	body := map[string]map[string]string{"transition": {"id": transitionID}}
	return a.jira.Do("POST", "/rest/api/3/issue/"+id+"/transitions", body, nil)
}

func (a *Client) SetGitHubState(number int, closed bool) error {
	body := map[string]string{"state": "open", "state_reason": "reopened"}
	if closed {
		// A Done-category Jira status cannot say why work ended, so "completed" is assumed.
		body = map[string]string{"state": "closed", "state_reason": "completed"}
	}
	return a.github.Do("PATCH", a.ghPath(number), body, nil)
}

func (a *Client) projectPath() string {
	p := a.c.ProjectStatus
	owner := "/orgs/"
	if p.OwnerType == "user" {
		owner = "/users/"
	}
	return owner + p.Owner + "/projectsV2/" + strconv.Itoa(p.ProjectNumber)
}

// projectPages reads every page of a cursor-paginated project listing or fails.
func projectPages[T any](a *Client, path string) ([]T, error) {
	var result []T
	for range a.c.MaxPages {
		var batch []T
		next, err := a.projects.DoPage(path, &batch)
		if err != nil {
			return nil, err
		}
		result = append(result, batch...)
		if next == "" {
			return result, nil
		}
		path = next
	}
	return nil, syncErr("Project pagination limit exceeded; no partial scan allowed")
}

func (a *Client) Project() (*ProjectSnapshot, error) {
	p := a.c.ProjectStatus
	fields, err := projectPages[apiProjectField](a, a.projectPath()+"/fields?per_page=100")
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(fields, func(f apiProjectField) bool { return f.Name == p.Field })
	if i < 0 || fields[i].DataType != "single_select" {
		return nil, syncErr("Project field %q is missing or not a single-select field", p.Field)
	}
	snap := &ProjectSnapshot{FieldID: fields[i].ID, Options: map[string]string{}, Items: map[int64]ProjectItem{}}
	for _, o := range fields[i].Options {
		snap.Options[o.Name.Raw] = o.ID
	}
	fieldID := strconv.FormatInt(snap.FieldID, 10)
	items, err := projectPages[apiProjectItem](a, a.projectPath()+"/items?per_page=100&fields="+fieldID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ContentType == "Issue" && item.Content != nil {
			snap.Items[item.Content.ID] = item.item(snap.FieldID)
		}
	}
	return snap, nil
}

func (a *Client) ProjectItem(itemID, fieldID int64) (ProjectItem, error) {
	var item apiProjectItem
	path := a.projectPath() + "/items/" + strconv.FormatInt(itemID, 10) + "?fields=" + strconv.FormatInt(fieldID, 10)
	if err := a.projects.Do("GET", path, nil, &item); err != nil {
		return ProjectItem{}, err
	}
	return item.item(fieldID), nil
}

func (a *Client) AddProjectItem(number int) error {
	owner, repo, _ := strings.Cut(a.c.Repository, "/")
	body := map[string]any{"type": "Issue", "owner": owner, "repo": repo, "number": number}
	return a.projects.Do("POST", a.projectPath()+"/items", body, nil)
}

func (a *Client) SetProjectStatus(itemID, fieldID int64, optionID string) error {
	body := map[string]any{"fields": []map[string]any{{"id": fieldID, "value": optionID}}}
	return a.projects.Do("PATCH", a.projectPath()+"/items/"+strconv.FormatInt(itemID, 10), body, nil)
}
