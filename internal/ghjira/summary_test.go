package ghjira

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testLinks = summaryLinks{github: "https://github.com/acme/test", jira: "https://acme.atlassian.net"}

func TestSummaryGroupsEntriesForReaders(t *testing.T) {
	t.Parallel()
	rep := &report{Mode: "apply", ExitCode: 2, PlannedWrites: new(3), DeferredWrites: new(1), Entries: []*Entry{
		{GitHub: 7, Kind: KindBlocked, Operation: "abc", Reason: "Unsupported | content\nhere"},
		{GitHub: 8, Kind: KindSyncParent, Destination: SideJira, Parent: "#2", Outcome: OutcomeRetry, Reason: "Parent changed after plan; retried next run"},
		{GitHub: 9, Kind: KindSyncProjectStatus, Destination: SideJira, Status: "Done"},
		{GitHub: 9, Kind: KindCreateComment, Destination: SideGitHub},
		{Kind: KindCreateGitHubIssue, JiraKey: "ENG-4"},
		{GitHub: 12, Kind: KindDeferred, Reason: "Over max_operations=20; planned again next run"},
		{GitHub: 10, Kind: KindNoop},
		{GitHub: 11, Kind: KindNoop},
	}}
	got := renderSummary(rep, testLinks)
	for _, want := range []string{
		"## GitHub ↔ Jira sync: ⚠️ Needs review (1)",
		"Mode: **apply** · planned writes: 3 · deferred to later runs (max_operations): 1",
		"| [#7](https://github.com/acme/test/issues/7) | Unsupported \\| content here | `abc` |",
		"### Retried next run",
		"| [#8](https://github.com/acme/test/issues/8) | Set parent on Jira to #2 | Parent changed after plan; retried next run |",
		"<summary>Changes made (3)</summary>",
		"| [#9](https://github.com/acme/test/issues/9) | Set Jira status to Done |",
		"| [#9](https://github.com/acme/test/issues/9) | Copy comment to GitHub |",
		"| [ENG-4](https://acme.atlassian.net/browse/ENG-4) | Create GitHub issue |",
		"<summary>Deferred to a later run (1)</summary>",
		"| [#12](https://github.com/acme/test/issues/12) | Over max_operations=20; planned again next run |",
		"| noop | 2 |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
}

func TestSummaryOfStoppedRunCallsChangesPlanned(t *testing.T) {
	t.Parallel()
	rep := &report{Mode: "apply", ExitCode: 1, Error: "Jira GET /x failed with HTTP 500", Entries: []*Entry{
		{GitHub: 1, Kind: KindSyncStatus, Destination: SideGitHub, Status: "closed"},
	}}
	got := renderSummary(rep, testLinks)
	for _, want := range []string{
		"## GitHub ↔ Jira sync: ❌ Failed",
		"> The run stopped: Jira GET /x failed with HTTP 500",
		"<summary>Planned changes (the run stopped before finishing them) (1)</summary>",
		"| Set status on GitHub to closed |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
}

func TestSummarizeReadsReportAndConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath, reportPath := filepath.Join(dir, "sync.json"), filepath.Join(dir, "report.json")
	must(t, os.WriteFile(configPath, mustMarshal(t, Object{
		"repository": "acme/test", "jira_url": "https://acme.atlassian.net",
		"jira_project": "ENG", "jira_default_issue_type": "10001",
	}), 0o600))
	must(t, os.WriteFile(reportPath, []byte(`{"mode":"dry-run","exit_code":0,"entries":[{"github":3,"kind":"create_issue","operation":"x"},`+
		`{"github":4,"kind":"sync_project_status","destination":"project","status":"Ready","operation":"y"}]}`), 0o600))
	var out bytes.Buffer
	if code := mainWith([]string{"-config", configPath, "-summarize", reportPath}, &out, nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	// Regression: a report naming the project as a destination could not be read back.
	for _, want := range []string{
		"✅ In sync", "<summary>Planned changes (dry run) (2)</summary>",
		"| [#3](https://github.com/acme/test/issues/3) | Create Jira issue |",
		"| [#4](https://github.com/acme/test/issues/4) | Set project status to Ready |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary is missing %q:\n%s", want, got)
		}
	}
	if code := mainWith([]string{"-config", configPath, "-summarize", filepath.Join(dir, "missing.json")}, &out, nil); code != 1 {
		t.Fatalf("missing report exited %d", code)
	}
}
