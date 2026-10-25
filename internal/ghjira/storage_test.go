package ghjira

import (
	"os/exec"
	"path/filepath"
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
	state2.Operations["example"] = Operation{Status: "pending"}
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
