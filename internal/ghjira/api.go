package ghjira

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	// FindIssue looks for an issue created by a pending intent journaled at started.
	FindIssue(source *GitHubIssue, started string) (*JiraIssue, error)
	// JiraIssue returns nil when the issue was deleted, hidden, or moved out of the project.
	JiraIssue(id string) (*JiraIssue, error)
	CreateIssue(source *GitHubIssue, parentKey string) (*JiraIssue, error)
	Comments(side Side, pair Pair) ([]Comment, error)
	Comment(side Side, pair Pair, id string) (Comment, error)
	CreateComment(side Side, pair Pair, text, op string) (Comment, error)
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
	// Parent sync. Parents are GitHub issue numbers in the synced repository; 0 means none.
	SubIssues(number int) ([]GitHubIssue, error)
	GitHubParent(number int) (int, error)
	SetGitHubParent(childID int64, parent, oldParent int) error
	// SetJiraParent sets a Jira issue's parent by key, or removes it when key is "".
	SetJiraParent(id, key string) error
	// Jira → GitHub creation.
	SelectedJiraIssues() ([]JiraIssue, error)
	CreateGitHubIssue(title, body string, labels []string, issueType string) (*GitHubIssue, error)
	// FindGitHubIssue looks for an issue a pending creation may have made, by its marker.
	FindGitHubIssue(op, started string) (*GitHubIssue, error)
	// Board backlog (project_status jira_board). Membership is by Jira issue ID.
	BacklogIssues() (map[string]bool, error)
	InBacklog(id string) (bool, error)
	MoveToBacklog(key string) error
	MoveToBoard(key string) error
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
		IssueTypes     []struct {
			ID      string `json:"id"`
			Subtask bool   `json:"subtask"`
		} `json:"issueTypes"`
	}
	if err := a.jira.Do("GET", "/rest/api/3/project/"+a.c.JiraProject, nil, &project); err != nil {
		return err
	}
	if project.ProjectTypeKey != "software" && project.ProjectTypeKey != "business" {
		return syncErr("Only Jira Software/business projects supported; service-desk privacy requires separate handling")
	}
	// A wrong type ID would fail every creation, or create issues of an unintended type.
	usable := map[string]bool{}
	for _, t := range project.IssueTypes {
		usable[t.ID] = !t.Subtask
	}
	configured := append([]string{a.c.JiraIssueType}, slices.Collect(maps.Values(a.c.JiraIssueTypes))...)
	for _, id := range configured {
		if !usable[id] {
			return syncErr("Jira issue type %s is not a standard issue type in project %s", id, a.c.JiraProject)
		}
	}
	if id := a.c.JiraSubtaskType; id != "" {
		if standard, found := usable[id]; !found || standard {
			return syncErr("jira_subtask_type %s is not a sub-task issue type in project %s", id, a.c.JiraProject)
		}
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
	err := a.jira.Do("GET", "/rest/api/3/issue/"+id+"?fields=project,labels,updated,status,parent,summary,description,issuetype&properties=gh-jira-sync", nil, issue)
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

// FindIssue looks for the Jira issue a pending creation may have made, by its
// identity property. started is when the intent was journaled: the search covers
// issues this account created since then, so Jira issues carry no visible marker.
// Intents journaled before started was recorded fall back to the label that older
// releases added.
func (a *Client) FindIssue(source *GitHubIssue, started string) (*JiraIssue, error) {
	op := token(a.c, OpIssue, source.idString())
	identity := IssueIdentity{Operation: op, GitHubID: source.idString()}
	jql := `project = "` + a.c.JiraProject + `" AND labels = "gh-sync-` + op + `"`
	if since, err := time.Parse(time.RFC3339, started); err == nil {
		// A margin covers clock skew between this runner and Jira.
		minutes := int(time.Since(since)/time.Minute) + 10
		jql = `project = "` + a.c.JiraProject + `" AND creator = currentUser() AND created >= -` +
			strconv.Itoa(minutes) + `m ORDER BY created ASC`
	}
	var found []string
	next := ""
	for page := 0; ; page++ {
		if page == a.c.MaxPages {
			return nil, syncErr("Jira search pagination limit exceeded; no partial scan allowed")
		}
		query := url.Values{"jql": {jql}, "maxResults": {"100"}, "fields": {"id"}, "properties": {"gh-jira-sync"}}
		if next != "" {
			query.Set("nextPageToken", next)
		}
		var result struct {
			Issues []struct {
				ID         string `json:"id"`
				Properties struct {
					Identity *IssueIdentity `json:"gh-jira-sync"`
				} `json:"properties"`
			} `json:"issues"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := a.jira.Do("GET", "/rest/api/3/search/jql?"+query.Encode(), nil, &result); err != nil {
			return nil, err
		}
		for _, issue := range result.Issues {
			if issue.Properties.Identity != nil && *issue.Properties.Identity == identity {
				found = append(found, issue.ID)
			}
		}
		if next = result.NextPageToken; next == "" {
			break
		}
	}
	if len(found) > 1 {
		return nil, syncErr("Multiple Jira issues have the same source identity")
	}
	if len(found) == 0 {
		return nil, nil
	}
	issue, err := a.JiraIssue(found[0])
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

// CreateIssue creates the Jira issue for a GitHub issue; with a parent key it is a
// sub-task under that issue, since Jira only takes a sub-task's parent at creation.
func (a *Client) CreateIssue(source *GitHubIssue, parentKey string) (*JiraIssue, error) {
	op := token(a.c, OpIssue, source.idString())
	// Extra fields are tenant-specific Jira fields, so they stay generic JSON.
	fields := Object{}
	maps.Copy(fields, a.c.JiraCreateFields)
	fields["project"] = Object{"key": a.c.JiraProject}
	fields["issuetype"] = Object{"id": a.c.issueTypeFor(source)}
	if parentKey != "" {
		fields["issuetype"] = Object{"id": a.c.JiraSubtaskType}
		fields["parent"] = Object{"key": parentKey}
	}
	fields["summary"] = source.Title
	fields["description"] = toADF(issueDescription(source))
	// Identity lives only in this issue property, written atomically with the issue.
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
	var listed []apiJiraComment
	start := 0
	for range a.c.MaxPages {
		q := url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {"100"}, "orderBy": {"created"}}
		var batch struct {
			Comments []apiJiraComment `json:"comments"`
			Total    int              `json:"total"`
		}
		if err := a.jira.Do("GET", "/rest/api/3/issue/"+pair.JiraID+"/comment?"+q.Encode(), nil, &batch); err != nil {
			return nil, err
		}
		listed = append(listed, batch.Comments...)
		start += len(batch.Comments)
		if start >= batch.Total {
			return a.withProperties(listed)
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
	err := a.jira.Do("GET", "/rest/api/3/issue/"+pair.JiraID+"/comment/"+id+"", nil, &j)
	if err != nil {
		return Comment{}, err
	}
	comments, err := a.withProperties([]apiJiraComment{j})
	if err != nil {
		return Comment{}, err
	}
	return comments[0], nil
}

func (a *Client) CreateComment(side Side, pair Pair, text, op string) (Comment, error) {
	if side == SideGitHub {
		var g apiGitHubComment
		err := a.github.Do("POST", a.ghPath(pair.GitHub)+"/comments", map[string]string{"body": text}, &g)
		return g.comment(), err
	}
	var j apiJiraComment
	// The copy's identity is a comment property set atomically on create, so the
	// copy itself shows no marker text.
	body := map[string]any{"body": toADF(text), "properties": []any{
		map[string]any{"key": "gh-jira-sync", "value": map[string]string{"operation": op}}}}
	err := a.jira.Do("POST", "/rest/api/3/issue/"+pair.JiraID+"/comment", body, &j)
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
	if snap.Items, err = a.projectItems(); err != nil {
		return nil, err
	}
	return snap, nil
}

// projectItemsQuery lists a project's items with their issue and single-select
// value. The REST item listing returns no items to GitHub App installation tokens
// (seen live), while GraphQL lists them all. Option IDs and item database IDs are
// the same ones the REST endpoints use.
const projectItemsQuery = `query($login: String!, $number: Int!, $field: String!, $cursor: String) {
  owner: %s(login: $login) {
    projectV2(number: $number) {
      items(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          fullDatabaseId
          isArchived
          content { __typename ... on Issue { fullDatabaseId } }
          fieldValueByName(name: $field) { ... on ProjectV2ItemFieldSingleSelectValue { optionId } }
        }
      }
    }
  }
}`

func (a *Client) projectItems() (map[int64]ProjectItem, error) {
	p := a.c.ProjectStatus
	ownerType := "organization"
	if p.OwnerType == "user" {
		ownerType = "user"
	}
	query := fmt.Sprintf(projectItemsQuery, ownerType)
	items := map[int64]ProjectItem{}
	var cursor *string
	for range a.c.MaxPages {
		body := map[string]any{"query": query, "variables": map[string]any{
			"login": p.Owner, "number": p.ProjectNumber, "field": p.Field, "cursor": cursor}}
		var resp struct {
			Data struct {
				Owner *struct {
					ProjectV2 *struct {
						Items struct {
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
							Nodes []struct {
								ID         string `json:"fullDatabaseId"`
								IsArchived bool   `json:"isArchived"`
								Content    *struct {
									Typename string `json:"__typename"`
									ID       string `json:"fullDatabaseId"`
								} `json:"content"`
								Value *struct {
									OptionID string `json:"optionId"`
								} `json:"fieldValueByName"`
							} `json:"nodes"`
						} `json:"items"`
					} `json:"projectV2"`
				} `json:"owner"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := a.projects.Do("POST", "/graphql", body, &resp); err != nil {
			return nil, err
		}
		if len(resp.Errors) > 0 {
			return nil, syncErr("GitHub GraphQL error reading the project: %s", resp.Errors[0].Message)
		}
		if resp.Data.Owner == nil || resp.Data.Owner.ProjectV2 == nil {
			return nil, syncErr("Project %d of %s is missing or not visible to the project token", p.ProjectNumber, p.Owner)
		}
		page := resp.Data.Owner.ProjectV2.Items
		for _, n := range page.Nodes {
			// Pull requests, draft issues, and items hidden from this token are skipped.
			if n.Content == nil || n.Content.Typename != "Issue" {
				continue
			}
			itemID, err1 := strconv.ParseInt(n.ID, 10, 64)
			issueID, err2 := strconv.ParseInt(n.Content.ID, 10, 64)
			if err1 != nil || err2 != nil {
				return nil, syncErr("Invalid project item ID from GitHub")
			}
			item := ProjectItem{ID: itemID, Archived: n.IsArchived}
			if n.Value != nil {
				item.OptionID = n.Value.OptionID
			}
			items[issueID] = item
		}
		if !page.PageInfo.HasNextPage {
			return items, nil
		}
		cursor = &page.PageInfo.EndCursor
	}
	return nil, syncErr("Project pagination limit exceeded; no partial scan allowed")
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

// withProperties fills in comment properties through the comment/list endpoint,
// which documents expanding them; listing an issue's comments does not. Properties
// carry copy identities and Service Management visibility, so a partial answer fails.
func (a *Client) withProperties(comments []apiJiraComment) ([]Comment, error) {
	props := map[string][]jiraProperty{}
	for start := 0; start < len(comments); start += 50 {
		batch := comments[start:min(start+50, len(comments))]
		ids := make([]int64, len(batch))
		for i, c := range batch {
			id, err := strconv.ParseInt(c.ID, 10, 64)
			if err != nil {
				return nil, syncErr("Invalid Jira comment ID")
			}
			ids[i] = id
		}
		var page struct {
			IsLast bool `json:"isLast"`
			Values []struct {
				ID         string         `json:"id"`
				Properties []jiraProperty `json:"properties"`
			} `json:"values"`
		}
		if err := a.jira.Do("POST", "/rest/api/3/comment/list?expand=properties", map[string][]int64{"ids": ids}, &page); err != nil {
			return nil, err
		}
		if !page.IsLast || len(page.Values) != len(batch) {
			return nil, syncErr("Incomplete Jira comment properties; no partial scan allowed")
		}
		for _, v := range page.Values {
			props[v.ID] = v.Properties
		}
	}
	result := make([]Comment, len(comments))
	for i, c := range comments {
		c.Properties = props[c.ID]
		result[i] = c.comment()
	}
	return result, nil
}

// inRepository reports an issue in the synced repository; sub-issues may live in
// other repositories of the same owner, which are not synced.
func (a *Client) inRepository(issue *GitHubIssue) bool {
	return strings.EqualFold(issue.RepositoryURL, a.c.GitHubURL+"/repos/"+a.c.Repository)
}

func (a *Client) SubIssues(number int) ([]GitHubIssue, error) {
	all, err := ghPages[GitHubIssue](a, a.ghPath(number)+"/sub_issues", nil)
	if err != nil {
		return nil, err
	}
	var result []GitHubIssue
	for _, child := range all {
		if a.inRepository(&child) {
			result = append(result, child)
		}
	}
	return result, nil
}

// GitHubParent returns the issue's parent number, or 0 when it has none or the
// parent is in another repository.
func (a *Client) GitHubParent(number int) (int, error) {
	parent := &GitHubIssue{}
	err := a.github.Do("GET", a.ghPath(number)+"/parent", nil, parent)
	if status, ok := httpStatus(err); ok && status == 404 {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !a.inRepository(parent) {
		return 0, nil
	}
	return parent.Number, nil
}

// SetGitHubParent makes the issue with ID childID a sub-issue of parent, replacing
// any current parent, or removes it from oldParent when parent is 0. GitHub takes the
// sub-issue's ID here, not its number.
func (a *Client) SetGitHubParent(childID int64, parent, oldParent int) error {
	if parent == 0 {
		return a.github.Do("DELETE", a.ghPath(oldParent)+"/sub_issue", map[string]int64{"sub_issue_id": childID}, nil)
	}
	body := map[string]any{"sub_issue_id": childID, "replace_parent": true}
	return a.github.Do("POST", a.ghPath(parent)+"/sub_issues", body, nil)
}

func (a *Client) SetJiraParent(id, key string) error {
	body := map[string]any{"fields": map[string]any{"parent": map[string]string{"key": key}}}
	if key == "" {
		// Jira documents removing a parent with update.parent.set.none = true.
		body = map[string]any{"update": map[string]any{"parent": []any{map[string]any{"set": map[string]bool{"none": true}}}}}
	}
	return a.jira.Do("PUT", "/rest/api/3/issue/"+id, body, nil)
}

// SelectedJiraIssues lists the project's Jira issues carrying jira_selection_label.
// issue_state applies as for GitHub: open excludes Jira's Done category, closed keeps
// only it. Every page is read or the run fails.
func (a *Client) SelectedJiraIssues() ([]JiraIssue, error) {
	jql := `project = "` + a.c.JiraProject + `" AND labels = "` + *a.c.JiraSelectionLabel + `"`
	switch a.c.IssueState {
	case "open":
		jql += " AND statusCategory != Done"
	case "closed":
		jql += " AND statusCategory = Done"
	}
	jql += " ORDER BY created ASC"
	var result []JiraIssue
	next := ""
	for range a.c.MaxPages {
		query := url.Values{"jql": {jql}, "maxResults": {"100"},
			"fields": {"project,summary,description,issuetype,status,updated,parent"}, "properties": {"gh-jira-sync"}}
		if next != "" {
			query.Set("nextPageToken", next)
		}
		var page struct {
			Issues        []JiraIssue `json:"issues"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := a.jira.Do("GET", "/rest/api/3/search/jql?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		result = append(result, page.Issues...)
		if next = page.NextPageToken; next == "" {
			return result, nil
		}
	}
	return nil, syncErr("Jira search pagination limit exceeded; no partial scan allowed")
}

func (a *Client) CreateGitHubIssue(title, body string, labels []string, issueType string) (*GitHubIssue, error) {
	request := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		request["labels"] = labels
	}
	if issueType != "" {
		request["type"] = issueType
	}
	created := &GitHubIssue{}
	if err := a.github.Do("POST", a.ghPath(0), request, created); err != nil {
		return nil, err
	}
	return created, nil
}

// FindGitHubIssue lists issues updated since the intent started (with a margin for
// clock skew) and returns the one whose body ends with the operation's hidden marker.
func (a *Client) FindGitHubIssue(op, started string) (*GitHubIssue, error) {
	since, err := time.Parse(time.RFC3339, started)
	if err != nil {
		return nil, syncErr("Pending Jira → GitHub creation has no start time; review the journal")
	}
	params := url.Values{"state": {"all"}, "since": {since.Add(-10 * time.Minute).UTC().Format(time.RFC3339)}}
	issues, err := ghPages[GitHubIssue](a, a.ghPath(0), params)
	if err != nil {
		return nil, err
	}
	var found []GitHubIssue
	for _, issue := range issues {
		if issue.PullRequest == nil && hasMarker(Comment{Side: SideGitHub, Body: issue.Body}, op) {
			found = append(found, issue)
		}
	}
	if len(found) > 1 {
		return nil, syncErr("Multiple GitHub issues carry the same Jira source identity")
	}
	if len(found) == 0 {
		return nil, nil
	}
	return &found[0], nil
}

func (a *Client) boardPath() string {
	return "/rest/agile/1.0/board/" + strconv.Itoa(a.c.ProjectStatus.JiraBoard)
}

type backlogPage struct {
	StartAt int `json:"startAt"`
	Total   int `json:"total"`
	Issues  []struct {
		ID string `json:"id"`
	} `json:"issues"`
}

// BacklogIssues returns the IDs of the issues in the board's backlog. Jira only lists
// incomplete issues there. Every page is read or the run fails.
func (a *Client) BacklogIssues() (map[string]bool, error) {
	ids := map[string]bool{}
	start := 0
	for range a.c.MaxPages {
		var page backlogPage
		q := url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {"100"}, "fields": {"id"}}
		if err := a.jira.Do("GET", a.boardPath()+"/backlog?"+q.Encode(), nil, &page); err != nil {
			if status, ok := httpStatus(err); ok && status == 400 {
				return nil, syncErr("Jira board %d has no backlog; enable it in the board's features", a.c.ProjectStatus.JiraBoard)
			}
			return nil, err
		}
		for _, issue := range page.Issues {
			ids[issue.ID] = true
		}
		start += len(page.Issues)
		if start >= page.Total {
			return ids, nil
		}
		if len(page.Issues) == 0 {
			return nil, syncErr("Incomplete Jira backlog pagination")
		}
	}
	return nil, syncErr("Jira backlog pagination limit exceeded; no partial scan allowed")
}

// InBacklog takes an issue ID or key.
func (a *Client) InBacklog(issue string) (bool, error) {
	var page backlogPage
	q := url.Values{"jql": {"issue = " + issue}, "maxResults": {"1"}, "fields": {"id"}}
	if err := a.jira.Do("GET", a.boardPath()+"/backlog?"+q.Encode(), nil, &page); err != nil {
		return false, err
	}
	return page.Total > 0, nil
}

// MoveToBacklog and MoveToBoard may answer 207 with per-issue failures; callers
// confirm the move by reading membership again.
func (a *Client) MoveToBacklog(key string) error {
	path := "/rest/agile/1.0/backlog/" + strconv.Itoa(a.c.ProjectStatus.JiraBoard) + "/issue"
	if err := a.jira.Do("POST", path, map[string][]string{"issues": {key}}, nil); err != nil {
		return err
	}
	return a.settleBacklog(key, true)
}

// settleBacklog waits until the backlog listing shows a move, which lags the write by
// about a second (observed live). Giving up is not an error: the caller's
// confirmation reports a move that never shows.
func (a *Client) settleBacklog(key string, want bool) error {
	for range 10 {
		in, err := a.InBacklog(key)
		if err != nil || in == want {
			return err
		}
		a.jira.sleep(time.Second)
	}
	return nil
}

// MoveToBoard puts an issue on the board; on a Kanban board with a backlog, Jira also
// moves it to the first column.
func (a *Client) MoveToBoard(key string) error {
	if err := a.jira.Do("POST", a.boardPath()+"/issue", map[string][]string{"issues": {key}}, nil); err != nil {
		return err
	}
	return a.settleBacklog(key, false)
}
