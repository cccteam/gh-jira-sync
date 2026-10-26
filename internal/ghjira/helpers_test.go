package ghjira

import (
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"
)

func testConfig(t *testing.T, extra Object) *Config {
	t.Helper()
	raw := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
		"jira_project": "ENG", "jira_issue_type": "10001"}
	maps.Copy(raw, extra)
	c, err := ParseConfig(canonical(raw, false))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// cp deep-copies a comment the way the real API hands out fresh objects: the raw
// body is re-decoded, so later edits to fake rows never leak into planned values.
func cp(c Comment) Comment {
	c.Body = decodeJSON(canonical(c.Body, false))
	return c
}

func ghComment(id int, text string) Comment {
	return Comment{Side: SideGitHub, ID: strconv.Itoa(id), Body: text, Created: "2026-01-01T00:00:00Z",
		Author: "alice", HTMLURL: "https://github.com/acme/test/issues/1#issuecomment-" + strconv.Itoa(id)}
}

func jiraComment(id int, text string) Comment {
	return Comment{Side: SideJira, ID: strconv.Itoa(id), Body: toADF(text), Created: "2026-01-01T00:00:00Z", Author: "Bob"}
}

type write struct {
	kind   string
	detail string
}

// fakeAPI simulates GitHub and Jira for one issue, with switches for failure modes.
type fakeAPI struct {
	source       GitHubIssue
	jira         *JiraIssue
	rows         map[Side][]Comment
	writes       []write
	loseResponse bool // perform the write, then fail as if the response was lost
	hideJira     bool // search lags behind writes
	githubGone   bool
	jiraGone     bool
	reads        []string
	listReads    []Side
	refuse       []int // HTTP statuses for upcoming writes to reject without effect
	transitions  []JiraTransition
	project      *ProjectSnapshot // the GitHub Project, in project status mode
	searches     []string         // started values passed to FindIssue
	itemClosed   bool             // mimic the project workflow: closing the issue sets Done
	backlog      bool             // the Jira issue is in the board's backlog
	boardColumn  *JiraStatus      // status a board move sets, like a Kanban board's first column
	nextID       int
}

func newFake() *fakeAPI {
	return &fakeAPI{
		source: GitHubIssue{ID: 42, Number: 1, Title: "Original title", Body: "description",
			HTMLURL: "https://github.com/acme/test/issues/1", State: "open"},
		transitions: []JiraTransition{{ID: "31", Name: "Done", To: jiraStatus("Done", "done")},
			{ID: "11", Name: "Reopen", To: jiraStatus("To Do", "new")}},
		rows:   map[Side][]Comment{SideGitHub: {}, SideJira: {}},
		nextID: 100,
	}
}

func (f *fakeAPI) maybeRefuse() error {
	if len(f.refuse) > 0 {
		status := f.refuse[0]
		f.refuse = f.refuse[1:]
		return &HTTPStatusError{Status: status, Msg: "refused"}
	}
	return nil
}

func (f *fakeAPI) lost() error {
	if f.loseResponse {
		return syncErr("lost response")
	}
	return nil
}

func (f *fakeAPI) issueCopy() *GitHubIssue {
	issue := f.source
	return &issue
}

func (f *fakeAPI) jiraCopy() *JiraIssue {
	if f.jira == nil {
		return nil
	}
	issue := *f.jira
	return &issue
}

func (f *fakeAPI) CheckProject() error { return nil }

func (f *fakeAPI) Issues() ([]GitHubIssue, error) {
	if f.githubGone {
		return nil, nil
	}
	return []GitHubIssue{f.source}, nil
}

func (f *fakeAPI) Issue(number int) (*GitHubIssue, error) {
	if f.githubGone || f.source.Number != number {
		return nil, nil
	}
	return f.issueCopy(), nil
}

func (f *fakeAPI) FindIssue(_ *GitHubIssue, started string) (*JiraIssue, error) {
	f.searches = append(f.searches, started)
	if f.hideJira {
		return nil, nil
	}
	return f.jiraCopy(), nil
}

func (f *fakeAPI) JiraIssue(id string) (*JiraIssue, error) {
	if f.jiraGone || f.jira == nil || f.jira.ID != id {
		return nil, nil
	}
	return f.jiraCopy(), nil
}

func (f *fakeAPI) CreateIssue(source *GitHubIssue, _ string) (*JiraIssue, error) {
	if err := f.maybeRefuse(); err != nil {
		return nil, err
	}
	f.jira = &JiraIssue{ID: "200", Key: "ENG-1"}
	f.jira.Properties.Identity = &IssueIdentity{Operation: token(fakeConfig, OpIssue, source.idString()), GitHubID: source.idString()}
	f.jira.Fields.Status = jiraStatus("To Do", "new")
	f.writes = append(f.writes, write{"issue", source.Title})
	if err := f.lost(); err != nil {
		return nil, err
	}
	return f.jiraCopy(), nil
}

func (f *fakeAPI) Comments(side Side, _ Pair) ([]Comment, error) {
	f.listReads = append(f.listReads, side)
	out := make([]Comment, len(f.rows[side]))
	for i, row := range f.rows[side] {
		out[i] = cp(row)
	}
	return out, nil
}

func (f *fakeAPI) find(side Side, id string) *Comment {
	for i := range f.rows[side] {
		if f.rows[side][i].ID == id {
			return &f.rows[side][i]
		}
	}
	return nil
}

func (f *fakeAPI) Comment(side Side, _ Pair, id string) (Comment, error) {
	f.reads = append(f.reads, string(side)+":"+id)
	row := f.find(side, id)
	if row == nil {
		return Comment{}, syncErr("missing comment")
	}
	return cp(*row), nil
}

func (f *fakeAPI) CreateComment(side Side, _ Pair, text, op string) (Comment, error) {
	if err := f.maybeRefuse(); err != nil {
		return Comment{}, err
	}
	f.nextID++
	row := ghComment(f.nextID, text)
	if side == SideJira {
		row = jiraComment(f.nextID, text)
		row.Identity = op // as the gh-jira-sync comment property would
	}
	f.rows[side] = append(f.rows[side], row)
	f.writes = append(f.writes, write{"create", string(side)})
	if err := f.lost(); err != nil {
		return Comment{}, err
	}
	return cp(row), nil
}

func (f *fakeAPI) EditComment(side Side, _ Pair, id, text string) error {
	if err := f.maybeRefuse(); err != nil {
		return err
	}
	row := f.find(side, id)
	if side == SideGitHub {
		row.Body = text
	} else {
		row.Body = toADF(text)
	}
	f.writes = append(f.writes, write{"edit", string(side)})
	return f.lost()
}

// syncer runs plan+apply like the CLI, keeping every checkpoint.
type syncer struct {
	t     *testing.T
	c     *Config
	api   *fakeAPI
	saved []*State
}

func newSyncer(t *testing.T) *syncer {
	return &syncer{t: t, c: testConfig(t, nil), api: newFake()}
}

func (x *syncer) save(s *State) error {
	x.saved = append(x.saved, s.clone())
	return nil
}

func (x *syncer) run(s *State) (*State, []*Entry, error) {
	if s == nil {
		s = NewState(x.c)
	}
	entries, err := Plan(x.c, s, x.api, time.Now())
	if err != nil {
		return nil, entries, err
	}
	result, err := Apply(x.c, s, x.api, entries, x.save)
	return result, entries, err
}

// ok runs a sync that must succeed.
func (x *syncer) ok(s *State) (*State, []*Entry) {
	x.t.Helper()
	result, entries, err := x.run(s)
	if err != nil {
		x.t.Fatalf("sync failed: %v", err)
	}
	return result, entries
}

// fails runs a sync that must fail.
func (x *syncer) fails(s *State) {
	x.t.Helper()
	if _, _, err := x.run(s); err == nil {
		x.t.Fatal("sync succeeded; expected an error")
	}
}

func (x *syncer) linked() *State {
	s, _ := x.ok(nil)
	return s
}

func (x *syncer) last() *State { return x.saved[len(x.saved)-1] }

func kinds(entries []*Entry) []Kind {
	out := make([]Kind, len(entries))
	for i, e := range entries {
		out[i] = e.Kind
	}
	return out
}

func lastWrite(f *fakeAPI) write { return f.writes[len(f.writes)-1] }

func jiraStatus(name, category string) JiraStatus {
	s := JiraStatus{Name: name}
	s.StatusCategory.Key = category
	return s
}

func (f *fakeAPI) JiraTransitions(string) ([]JiraTransition, error) {
	return slices.Clone(f.transitions), nil
}

func (f *fakeAPI) TransitionJira(_ string, transitionID string) error {
	if err := f.maybeRefuse(); err != nil {
		return err
	}
	i := slices.IndexFunc(f.transitions, func(t JiraTransition) bool { return t.ID == transitionID })
	f.jira.Fields.Status = f.transitions[i].To
	f.writes = append(f.writes, write{"transition", f.transitions[i].Name})
	return f.lost()
}

func (f *fakeAPI) SetGitHubState(_ int, closed bool) error {
	if err := f.maybeRefuse(); err != nil {
		return err
	}
	f.source.State = statusName(closed)
	if closed && f.itemClosed {
		item := f.project.Items[f.source.ID]
		item.OptionID = f.project.Options["Done"]
		f.project.Items[f.source.ID] = item
	}
	f.writes = append(f.writes, write{"state", f.source.State})
	return f.lost()
}

func (f *fakeAPI) Project() (*ProjectSnapshot, error) {
	snap := *f.project
	snap.Options = maps.Clone(f.project.Options)
	snap.Items = maps.Clone(f.project.Items)
	return &snap, nil
}

func (f *fakeAPI) ProjectItem(itemID, _ int64) (ProjectItem, error) {
	for _, item := range f.project.Items {
		if item.ID == itemID {
			return item, nil
		}
	}
	return ProjectItem{}, &HTTPStatusError{Status: 404, Msg: "no item"}
}

func (f *fakeAPI) AddProjectItem(int) error {
	if err := f.maybeRefuse(); err != nil {
		return err
	}
	if _, ok := f.project.Items[f.source.ID]; !ok {
		f.project.Items[f.source.ID] = ProjectItem{ID: 900}
	}
	f.writes = append(f.writes, write{"add", "project"})
	return f.lost()
}

func (f *fakeAPI) SetProjectStatus(itemID, _ int64, optionID string) error {
	if err := f.maybeRefuse(); err != nil {
		return err
	}
	for issue, item := range f.project.Items {
		if item.ID == itemID {
			item.OptionID = optionID
			f.project.Items[issue] = item
		}
	}
	f.writes = append(f.writes, write{"project", f.project.optionName(optionID)})
	return f.lost()
}

func (f *fakeAPI) BacklogIssues() (map[string]bool, error) {
	if f.jira == nil {
		return map[string]bool{}, nil
	}
	return map[string]bool{f.jira.ID: f.backlog}, nil
}

func (f *fakeAPI) InBacklog(string) (bool, error) { return f.backlog, nil }

func (f *fakeAPI) MoveToBacklog(key string) error {
	if err := f.maybeRefuse(); err != nil {
		return err
	}
	f.backlog = true
	f.writes = append(f.writes, write{"backlog", key})
	return f.lost()
}

func (f *fakeAPI) MoveToBoard(key string) error {
	if err := f.maybeRefuse(); err != nil {
		return err
	}
	f.backlog = false
	if f.boardColumn != nil {
		f.jira.Fields.Status = *f.boardColumn
	}
	f.writes = append(f.writes, write{"board", key})
	return f.lost()
}

// fakeConfig has the binding every test config shares, so the fake can compute the
// identity Jira would store on a created issue.
var fakeConfig = func() *Config {
	c, err := ParseConfig([]byte(`{"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
		"jira_project": "ENG", "jira_issue_type": "10001"}`))
	if err != nil {
		panic(err)
	}
	return c
}()

// Parent sync on the single-issue fake: never any parent.
func (f *fakeAPI) SubIssues(int) ([]GitHubIssue, error)  { return nil, nil }
func (f *fakeAPI) GitHubParent(int) (int, error)         { return 0, nil }
func (f *fakeAPI) SetGitHubParent(int64, int, int) error { return nil }
func (f *fakeAPI) SetJiraParent(string, string) error    { return nil }

// Jira → GitHub creation on the single-issue fake: nothing is selected in Jira.
func (f *fakeAPI) SelectedJiraIssues() ([]JiraIssue, error) { return nil, nil }
func (f *fakeAPI) CreateGitHubIssue(string, string, []string, string) (*GitHubIssue, error) {
	return nil, syncErr("unexpected GitHub issue creation")
}
func (f *fakeAPI) FindGitHubIssue(string, string) (*GitHubIssue, error) { return nil, nil }
