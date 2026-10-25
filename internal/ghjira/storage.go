package ghjira

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// State is the operation journal and mapping store (format version 2).
type State struct {
	Version    int                  `json:"version"`
	Binding    string               `json:"binding"`
	Pairs      map[string]Pair      `json:"pairs"`
	Operations map[string]Operation `json:"operations"`
	Comments   map[string]Link      `json:"comments"`
	// Acknowledged maps an operation to the reviewed reason it is silenced for.
	Acknowledged map[string]string `json:"acknowledged"`
	// Reconciled holds issue timestamps of pairs whose last full scan found nothing to do.
	Reconciled map[string]Stamp `json:"reconciled"`
}

// Pair links a GitHub issue (keyed by number) to its Jira issue.
type Pair struct {
	GitHub   int    `json:"github"`
	GitHubID string `json:"github_id"`
	JiraID   string `json:"jira_id"`
	JiraKey  string `json:"jira_key"`
	// Closed is the status both sides last agreed on; nil until status sync first
	// sees them agree. The side that differs from it is the side that changed.
	Closed *bool `json:"closed,omitempty"`
	// ProjectStatus is the project option both sides last agreed on, in project
	// status mode; nil until they first agree. It works like Closed.
	ProjectStatus *string `json:"project_status,omitempty"`
}

// Operation is a journaled create: pending before dispatch, done after confirmation,
// rejected when the service refused it with a 4xx.
type Operation struct {
	Kind       OpKind `json:"kind"`
	SourceID   string `json:"source_id"`
	GitHub     int    `json:"github"`
	Status     Status `json:"status"`
	Prefix     string `json:"prefix,omitempty"`
	Suffix     string `json:"suffix,omitempty"`
	Baseline   string `json:"baseline,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	// Started is when an issue creation was journaled (RFC 3339, UTC). Recovery
	// searches for issues created since then.
	Started string `json:"started,omitempty"`
}

// Link maps an original comment to its copy with the last confirmed fingerprints.
type Link struct {
	GitHub   int    `json:"github"`
	Origin   Side   `json:"origin"` // side of the original; the other side holds the copy
	Prefix   string `json:"prefix"`
	Suffix   string `json:"suffix"`
	Baseline string `json:"baseline"`
	CopyHash string `json:"copy_hash"`
	GitHubID string `json:"github_id"`
	JiraID   string `json:"jira_id"`
}

func (l Link) id(side Side) string {
	if side == SideGitHub {
		return l.GitHubID
	}
	return l.JiraID
}

func (l *Link) setID(side Side, id string) {
	if side == SideGitHub {
		l.GitHubID = id
	} else {
		l.JiraID = id
	}
}

// Stamp records issue timestamps at the time of a clean full scan.
type Stamp struct {
	GitHub  string `json:"github"`
	Jira    string `json:"jira"`
	Scanned string `json:"scanned"`
}

// ParseState decodes state strictly; the semantic checks are in validateState.
func ParseState(data []byte) (*State, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, err
	}
	allowed := map[string]bool{"version": true, "binding": true, "pairs": true, "operations": true,
		"comments": true, "acknowledged": true, "reconciled": true}
	for k := range keys {
		if !allowed[k] {
			return nil, syncErr("Invalid state version or endpoint/project binding; restore correct state")
		}
	}
	for _, k := range []string{"version", "binding", "pairs", "operations", "comments"} {
		raw, present := keys[k]
		if !present {
			return nil, syncErr("Invalid state version or endpoint/project binding; restore correct state")
		}
		if k != "version" && k != "binding" && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			return nil, syncErr("Invalid state structure")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	s := &State{}
	if err := dec.Decode(s); err != nil {
		return nil, syncErr("Invalid state structure")
	}
	s.ensureMaps()
	return s, nil
}

func (s *State) ensureMaps() {
	if s.Pairs == nil {
		s.Pairs = map[string]Pair{}
	}
	if s.Operations == nil {
		s.Operations = map[string]Operation{}
	}
	if s.Comments == nil {
		s.Comments = map[string]Link{}
	}
	if s.Acknowledged == nil {
		s.Acknowledged = map[string]string{}
	}
	if s.Reconciled == nil {
		s.Reconciled = map[string]Stamp{}
	}
}

// clone deep-copies state; every map holds plain values.
func (s *State) clone() *State {
	return &State{Version: s.Version, Binding: s.Binding, Pairs: cloneMap(s.Pairs),
		Operations: cloneMap(s.Operations), Comments: cloneMap(s.Comments),
		Acknowledged: cloneMap(s.Acknowledged), Reconciled: cloneMap(s.Reconciled)}
}

func cloneMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	maps.Copy(out, m)
	return out
}

// atomicJSON writes sorted, indented JSON durably: temp file, fsync, rename, fsync dir.
func atomicJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".sync-")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp) // no-op after a successful rename
	_, err = f.Write(append(canonical(v, true), '\n'))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp, path)
	}
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// GitStore checkpoints state to a dedicated branch on origin. It never checks out,
// force-pushes, or retries: a rejected push means a concurrent writer.
type GitStore struct {
	Branch   string
	Dir      string // working directory for git; empty means the current directory
	parent   string
	previous []byte
}

// stateBranchPattern allows "sync-state" for a single deployment and
// "sync-state-<name>" for several in one repository. A slash would collide with an
// existing "sync-state" branch in Git's ref namespace, so names use a dash.
var stateBranchPattern = regexp.MustCompile(`^sync-state(-[A-Za-z0-9][A-Za-z0-9_-]*)?$`)

func NewGitStore(branch string) (*GitStore, error) {
	if !stateBranchPattern.MatchString(branch) {
		return nil, syncErr("State branch must be sync-state or sync-state-<name>")
	}
	return &GitStore{Branch: branch}, nil
}

func (g *GitStore) git(data string, allowed []int, args ...string) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=github-jira-sync[bot]", "GIT_AUTHOR_EMAIL=github-jira-sync[bot]@users.noreply.github.com",
		"GIT_COMMITTER_NAME=github-jira-sync[bot]", "GIT_COMMITTER_EMAIL=github-jira-sync[bot]@users.noreply.github.com")
	cmd.Stdin = strings.NewReader(data)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || ctx.Err() != nil {
			return 0, "", syncErr("Git checkpoint failed or outcome unknown; stop and reload state")
		}
		code = exit.ExitCode()
	}
	if slices.Contains(allowed, code) {
		return code, strings.TrimSpace(stdout.String()), nil
	}
	return code, "", syncErr("Git checkpoint failed; check remote, permissions, or concurrent writer")
}

// Load returns nil when the branch does not exist yet.
func (g *GitStore) Load() (*State, error) {
	// The full ref avoids matching another branch that merely ends in the same name.
	code, _, err := g.git("", []int{0, 2}, "ls-remote", "--exit-code", "origin", "refs/heads/"+g.Branch)
	if err != nil || code == 2 {
		return nil, err
	}
	if _, _, err := g.git("", []int{0}, "fetch", "origin", "refs/heads/"+g.Branch); err != nil {
		return nil, err
	}
	_, parent, err := g.git("", []int{0}, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return nil, err
	}
	// Each checkpoint replaces the whole tree with state.json, so pointing the store at
	// a branch holding anything else would wipe that branch's files on the next save.
	if _, files, err := g.git("", []int{0}, "ls-tree", "--name-only", parent); err != nil || files != "state.json" {
		if err == nil {
			err = syncErr("Branch %s holds more than state.json; refusing to use it as sync state", g.Branch)
		}
		return nil, err
	}
	_, raw, err := g.git("", []int{0}, "show", parent+":state.json")
	if err != nil {
		return nil, err
	}
	s, err := ParseState([]byte(raw))
	if err != nil {
		return nil, err
	}
	g.parent, g.previous = parent, canonical(s, false)
	return s, nil
}

// Save commits state on top of the loaded parent and pushes it; unchanged state is skipped.
func (g *GitStore) Save(s *State) error {
	current := canonical(s, false)
	if g.previous != nil && bytes.Equal(current, g.previous) {
		return nil
	}
	_, blob, err := g.git(string(canonical(s, true))+"\n", []int{0}, "hash-object", "-w", "--stdin")
	if err != nil {
		return err
	}
	_, tree, err := g.git("100644 blob "+blob+"\tstate.json\n", []int{0}, "mktree")
	if err != nil {
		return err
	}
	args := []string{"commit-tree", tree}
	if g.parent != "" {
		args = append(args, "-p", g.parent)
	}
	args = append(args, "-m", "Checkpoint sync operation journal "+uuid4())
	_, commit, err := g.git("", []int{0}, args...)
	if err != nil {
		return err
	}
	// A normal push must reject conflicting writers; never retry or force it.
	if _, _, err := g.git("", []int{0}, "push", "origin", commit+":refs/heads/"+g.Branch); err != nil {
		return err
	}
	g.parent, g.previous = commit, current
	return nil
}

// uuid4 makes every checkpoint commit unique, even for identical payloads.
func uuid4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
