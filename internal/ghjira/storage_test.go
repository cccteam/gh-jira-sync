package ghjira

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealGitPersistenceReloadAndCompetingWriter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	remote, local := filepath.Join(root, "remote.git"), filepath.Join(root, "local")
	for _, args := range [][]string{{"init", "--bare", remote}, {"clone", remote, local}} {
		if out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	store := func() *GitStore { return &GitStore{Branch: "sync-state", Dir: local} }
	loaded := func() *State {
		s, err := store().Load()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := store(), store()
	for _, s := range []*GitStore{a, b} {
		if state, err := s.Load(); err != nil || state != nil {
			t.Fatalf("empty remote: %v %v", state, err)
		}
	}
	state := NewState(testConfig(t, nil))
	if err := a.Save(state); err != nil {
		t.Fatal(err)
	}
	eq(t, string(canonical(loaded(), false)), string(canonical(state, false)))
	// Even identical content from a competing initial writer must fail.
	if b.Save(state) == nil {
		t.Fatal("competing writer pushed")
	}
	state2 := state.clone()
	state2.Operations["example"] = Operation{Kind: OpIssue, SourceID: "42", GitHub: 1, Status: StatusPending}
	if b.Save(state2) == nil {
		t.Fatal("competing writer pushed")
	}
	if err := a.Save(state2); err != nil {
		t.Fatal(err)
	}
	eq(t, string(canonical(loaded(), false)), string(canonical(state2, false)))
}

func TestOnlySyncStateBranchSupported(t *testing.T) {
	t.Parallel()
	if _, err := NewGitStore("main"); err == nil {
		t.Fatal("other branches must be rejected")
	}
}

func TestStateRejectsUnknownEnumValues(t *testing.T) {
	t.Parallel()
	valid := NewState(testConfig(t, nil))
	op := token(testConfig(t, nil), OpIssue, "42")
	valid.Operations[op] = Operation{Kind: OpIssue, SourceID: "42", GitHub: 1, Status: StatusPending}
	base := string(canonical(valid, false))
	if _, err := ParseState([]byte(base)); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}
	for _, bad := range []struct{ from, to string }{
		{`"status":"pending"`, `"status":"bogus"`},
		{`"kind":"issue"`, `"kind":"comment_gitlab"`},
	} {
		if _, err := ParseState([]byte(strings.Replace(base, bad.from, bad.to, 1))); err == nil {
			t.Errorf("state with %s accepted", bad.to)
		}
	}
	link := `{"version":2,"binding":"x","pairs":{},"operations":{},"comments":{"op":{"github":1,"origin":"gitlab",
		"prefix":"","suffix":"","baseline":"","copy_hash":"","github_id":"1","jira_id":"1"}}}`
	if _, err := ParseState([]byte(link)); err == nil {
		t.Error("link with unknown origin accepted")
	}
}

// gitClone returns a working clone of a fresh bare remote.
func gitClone(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote, local := filepath.Join(root, "remote.git"), filepath.Join(root, "local")
	for _, args := range [][]string{{"init", "--bare", remote}, {"clone", remote, local}} {
		if out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return local
}

func TestStateBranchNames(t *testing.T) {
	t.Parallel()
	for name, valid := range map[string]bool{
		"sync-state": true, "sync-state-team-a": true, "sync-state-A_1": true,
		"main": false, "sync-state/team-a": false, "sync-state-": false, "sync-state-.lock": false,
		"sync-states": false, "sync-state-a/b": false, "refs/heads/sync-state": false,
	} {
		if _, err := NewGitStore(name); (err == nil) != valid {
			t.Errorf("%q: valid=%v, err=%v", name, valid, err)
		}
	}
}

func TestSeveralDeploymentsShareOneRemote(t *testing.T) {
	t.Parallel()
	local := gitClone(t)
	states := map[string]*State{}
	for _, d := range []struct{ branch, project string }{{"sync-state", "ENG"}, {"sync-state-ops", "OPS"}} {
		store := &GitStore{Branch: d.branch, Dir: local}
		if s, err := store.Load(); err != nil || s != nil {
			t.Fatalf("%s: expected no state yet: %v %v", d.branch, s, err)
		}
		states[d.branch] = NewState(testConfig(t, Object{"jira_project": d.project}))
		if err := store.Save(states[d.branch]); err != nil {
			t.Fatalf("%s: %v", d.branch, err)
		}
	}
	for branch, want := range states {
		got, err := (&GitStore{Branch: branch, Dir: local}).Load()
		if err != nil {
			t.Fatal(err)
		}
		eq(t, got.Binding, want.Binding)
	}
}

func TestRefusesBranchHoldingMoreThanState(t *testing.T) {
	t.Parallel()
	local := gitClone(t)
	for _, name := range []string{"state.json", "README.md"} {
		if err := os.WriteFile(filepath.Join(local, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"checkout", "-q", "-b", "sync-state-docs"},
		{"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "docs"},
		{"push", "-q", "origin", "sync-state-docs"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = local
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	_, err := (&GitStore{Branch: "sync-state-docs", Dir: local}).Load()
	if err == nil || !strings.Contains(err.Error(), "holds more than state.json") {
		t.Fatalf("expected refusal, got %v", err)
	}
}
