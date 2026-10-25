package ghjira

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// cliRun prepares config, state, and report paths in a temp dir with the fake API wired in.
type cliRun struct {
	t                     *testing.T
	api                   *fakeAPI
	config, state, report string
}

func newCLI(t *testing.T) *cliRun {
	dir := t.TempDir()
	r := &cliRun{t: t, api: newFake(), config: filepath.Join(dir, "config.json"),
		state: filepath.Join(dir, "state.json"), report: filepath.Join(dir, "report.json")}
	raw := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
		"jira_project": "ENG", "jira_issue_type": "10001"}
	if err := atomicJSON(r.config, raw); err != nil {
		t.Fatal(err)
	}
	original := newAPI
	newAPI = func(*Config) (API, error) { return r.api, nil }
	t.Cleanup(func() { newAPI = original })
	return r
}

func (r *cliRun) main(extra ...string) int {
	args := append([]string{"--config", r.config, "--state", r.state, "--report", r.report}, extra...)
	return Main(args, io.Discard)
}

func (r *cliRun) readReport() map[string]any {
	data, err := os.ReadFile(r.report)
	if err != nil {
		r.t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(data, &rep); err != nil {
		r.t.Fatal(err)
	}
	return rep
}

func firstEntry(rep map[string]any) map[string]any {
	return rep["entries"].([]any)[0].(map[string]any)
}

func TestCLIDryRunDoesNotCreateStateOrWrite(t *testing.T) {
	r := newCLI(t)
	eq(t, r.main("--init-state"), 0)
	if _, err := os.Stat(r.state); err == nil {
		t.Fatal("dry run created state")
	}
	eq(t, len(r.api.writes), 0)
	eq(t, r.readReport()["planned_writes"], any(1.0))
}

func TestMissingStateFailsBeforeNetwork(t *testing.T) {
	r := newCLI(t)
	called := false
	newAPI = func(*Config) (API, error) { called = true; return r.api, nil }
	eq(t, r.main("--apply"), 1)
	eq(t, called, false)
}

func TestCLIApplyThenRepeat(t *testing.T) {
	r := newCLI(t)
	eq(t, r.main("--apply", "--init-state"), 0)
	eq(t, r.main("--apply"), 0)
	eq(t, len(r.api.writes), 1)
}

func TestCLIAcknowledgeClearsReviewExitCode(t *testing.T) {
	r := newCLI(t)
	eq(t, r.main("--apply", "--init-state"), 0)
	r.api.rows["github"] = []Comment{ghComment(11, "Original")}
	eq(t, r.main("--apply"), 0)
	r.api.rows["github"] = nil
	eq(t, r.main("--apply"), 2)
	op := firstEntry(r.readReport())["operation"].(string)
	eq(t, r.main("--apply", "--acknowledge", op), 0)
	eq(t, r.main("--apply"), 0)
	eq(t, firstEntry(r.readReport())["kind"], any("acknowledged"))
}

func TestCLIReportIncludesApplyOutcomes(t *testing.T) {
	r := newCLI(t)
	r.api.refuse = []int{400}
	eq(t, r.main("--apply", "--init-state"), 2)
	eq(t, firstEntry(r.readReport())["outcome"], any("rejected"))
}

func TestCLIRejectsSharedPathsAndBadBranch(t *testing.T) {
	r := newCLI(t)
	eq(t, Main([]string{"--config", r.config, "--state", r.config}, io.Discard), 1)
	eq(t, r.main("--git-state-branch", "main"), 2)
}

func TestLockExcludesOtherWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := locked(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := locked(path); err == nil {
		t.Fatal("second writer acquired the lock")
	}
}

func TestCLIReportsMissingConfigPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.json")
	report := filepath.Join(dir, "report.json")
	eq(t, Main([]string{"--config", missing, "--state", filepath.Join(dir, "s.json"), "--report", report}, io.Discard), 1)
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	eq(t, rep["error"], any("Cannot open "+missing+": no such file or directory"))
}
