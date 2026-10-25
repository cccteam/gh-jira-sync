package ghjira

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealGitPersistenceReloadAndCompetingWriter(t *testing.T) {
	root := t.TempDir()
	remote, local := filepath.Join(root, "remote.git"), filepath.Join(root, "local")
	for _, args := range [][]string{{"init", "--bare", remote}, {"clone", remote, local}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
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
	if _, err := NewGitStore("main"); err == nil {
		t.Fatal("other branches must be rejected")
	}
}

func TestStateRejectsUnknownEnumValues(t *testing.T) {
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
