package ghjira

import (
	"strconv"
	"testing"
	"time"
)

func testConfig(t *testing.T, extra Object) *Config {
	t.Helper()
	raw := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
		"jira_project": "ENG", "jira_issue_type": "10001"}
	for k, v := range extra {
		raw[k] = v
	}
	c, err := ParseConfig(canonical(raw, false))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// cp deep-copies JSON-shaped values the way the real API hands out fresh objects.
func cp(o Object) Object {
	if o == nil {
		return nil
	}
	return decodeJSON(canonical(o, false)).(Object)
}

func ghComment(id int, text string) Object {
	return Object{"id": id, "body": text, "created_at": "2026-01-01T00:00:00Z",
		"updated_at": "2026-01-01T00:00:00Z", "user": Object{"login": "alice"},
		"html_url": "https://github.com/acme/test/issues/1#issuecomment-" + strconv.Itoa(id)}
}

func jiraComment(id int, text string) Object {
	return Object{"id": strconv.Itoa(id), "body": toADF(text), "created": "2026-01-01T00:00:00Z",
		"updated": "2026-01-01T00:00:00Z", "author": Object{"displayName": "Bob"}}
}

type write struct{ kind, detail string }

// fakeAPI simulates GitHub and Jira for one issue, with switches for failure modes.
type fakeAPI struct {
	source       Object
	jira         Object
	rows         map[string][]Object
	writes       []write
	loseResponse bool // perform the write, then fail as if the response was lost
	hideJira     bool // search lags behind writes
	githubGone   bool
	jiraGone     bool
	reads        []string
	listReads    []string
	refuse       []int // HTTP statuses for upcoming writes to reject without effect
	nextID       int
}

func newFake() *fakeAPI {
	return &fakeAPI{
		source: Object{"id": 42, "number": 1, "title": "Original title", "body": "description",
			"html_url": "https://github.com/acme/test/issues/1", "labels": []any{Object{"name": "sync-jira"}}},
		rows:   map[string][]Object{"github": {}, "jira": {}},
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

func (f *fakeAPI) CheckProject() error { return nil }

func (f *fakeAPI) Issues() ([]Object, error) {
	if f.githubGone {
		return nil, nil
	}
	return []Object{cp(f.source)}, nil
}

func (f *fakeAPI) Issue(number int) (Object, error) {
	if f.githubGone || idString(f.source["number"]) != strconv.Itoa(number) {
		return nil, nil
	}
	return cp(f.source), nil
}

func (f *fakeAPI) FindIssue(Object) (Object, error) {
	if f.hideJira {
		return nil, nil
	}
	return cp(f.jira), nil
}

func (f *fakeAPI) JiraIssue(id string) (Object, error) {
	if f.jiraGone || f.jira == nil || idString(f.jira["id"]) != id {
		return nil, nil
	}
	return cp(f.jira), nil
}

func (f *fakeAPI) CreateIssue(source Object) (Object, error) {
	if err := f.maybeRefuse(); err != nil {
		return nil, err
	}
	f.jira = Object{"id": "200", "key": "ENG-1"}
	title, _ := source["title"].(string)
	f.writes = append(f.writes, write{"issue", title})
	if err := f.lost(); err != nil {
		return nil, err
	}
	return cp(f.jira), nil
}

func (f *fakeAPI) Comments(side string, _ Pair) ([]Object, error) {
	f.listReads = append(f.listReads, side)
	out := make([]Object, len(f.rows[side]))
	for i, row := range f.rows[side] {
		out[i] = cp(row)
	}
	return out, nil
}

func (f *fakeAPI) find(side, id string) Object {
	for _, row := range f.rows[side] {
		if idString(row["id"]) == id {
			return row
		}
	}
	return nil
}

func (f *fakeAPI) Comment(side string, _ Pair, id string) (Object, error) {
	f.reads = append(f.reads, side+":"+id)
	row := f.find(side, id)
	if row == nil {
		return nil, syncErr("missing comment")
	}
	return cp(row), nil
}

func (f *fakeAPI) CreateComment(side string, _ Pair, text string) (Object, error) {
	if err := f.maybeRefuse(); err != nil {
		return nil, err
	}
	f.nextID++
	raw := ghComment(f.nextID, text)
	if side == "jira" {
		raw = jiraComment(f.nextID, text)
	}
	f.rows[side] = append(f.rows[side], raw)
	f.writes = append(f.writes, write{"create", side})
	if err := f.lost(); err != nil {
		return nil, err
	}
	return cp(raw), nil
}

func (f *fakeAPI) EditComment(side string, _ Pair, id, text string) error {
	if err := f.maybeRefuse(); err != nil {
		return err
	}
	row := f.find(side, id)
	if side == "github" {
		row["body"] = text
	} else {
		row["body"] = toADF(text)
	}
	f.writes = append(f.writes, write{"edit", side})
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

func kinds(entries []*Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Kind
	}
	return out
}

func lastWrite(f *fakeAPI) write { return f.writes[len(f.writes)-1] }
