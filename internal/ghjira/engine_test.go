package ghjira

import (
	"cmp"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func eq[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func sorted[T cmp.Ordered](s []T) []T {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func TestCreationThenRepeatNoop(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	eq(t, s.Pairs["1"].JiraKey, "ENG-1")
	x.ok(s)
	eq(t, x.api.writes, []write{{"issue", "Original title"}})
	pending := false
	for _, saved := range x.saved {
		for _, op := range saved.Operations {
			pending = pending || op.Status == "pending"
		}
	}
	if !pending {
		t.Fatal("creation intent was never journaled as pending")
	}
}

func TestNeverSyncLaterIssueFieldEdits(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.source.Title = "New title"
	x.ok(s)
	eq(t, len(x.api.writes), 1)
}

func TestCreationLostResponseRecoveredByIdentity(t *testing.T) {
	x := newSyncer(t)
	x.api.loseResponse = true
	x.fails(nil)
	pending := x.last()
	x.api.loseResponse = false
	s, entries := x.ok(pending)
	eq(t, entries[0].Kind, "link_issue")
	eq(t, len(x.api.writes), 1)
	if _, ok := s.Pairs["1"]; !ok {
		t.Fatal("pair not recovered")
	}
}

func TestUnknownCreationNeverRetriedWhenSearchLags(t *testing.T) {
	x := newSyncer(t)
	x.api.loseResponse = true
	x.fails(nil)
	x.api.hideJira = true
	_, entries := x.ok(x.last())
	eq(t, entries[0].Kind, "blocked")
	eq(t, len(x.api.writes), 1)
}

func TestJournalFailurePreventsDispatch(t *testing.T) {
	x := newSyncer(t)
	entries, err := Plan(x.c, NewState(x.c), x.api, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	save := func(*State) error {
		calls++
		if calls == 2 {
			return syncErr("state push failed")
		}
		return nil
	}
	if _, err := Apply(x.c, NewState(x.c), x.api, entries, save); err == nil {
		t.Fatal("expected the failed intent checkpoint to stop the run")
	}
	eq(t, len(x.api.writes), 0)
}

func TestTwoWayCommentsAndNoEcho(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "GitHub original")}
	x.api.rows["jira"] = []Comment{jiraComment(22, "Jira original")}
	s, _ = x.ok(s)
	eq(t, len(s.Comments), 2)
	eq(t, x.api.writes[1:], []write{{"create", "jira"}, {"create", "github"}})
	x.ok(s)
	eq(t, len(x.api.writes), 3)
}

func TestEditOriginalsBothDirections(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "GitHub original")}
	x.api.rows["jira"] = []Comment{jiraComment(22, "Jira original")}
	s, _ = x.ok(s)
	x.api.rows["github"][0].Body = "GitHub edited"
	x.api.rows["jira"][0].Body = toADF("Jira edited")
	s, _ = x.ok(s)
	eq(t, x.api.writes[len(x.api.writes)-2:], []write{{"edit", "jira"}, {"edit", "github"}})
	x.ok(s)
	eq(t, len(x.api.writes), 5)
}

func jiraText(t *testing.T, row Comment) string {
	t.Helper()
	text, err := adfText(row.Body)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestEditCopyIsReportedNeverOverwritten(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	s, _ = x.ok(s)
	x.api.rows["jira"][0].Body = toADF(strings.Replace(jiraText(t, x.api.rows["jira"][0]), "Original", "Changed copy", 1))
	x.api.rows["github"][0].Body = "Changed original too"
	_, entries := x.ok(s)
	eq(t, entries[0].Kind, "blocked")
	eq(t, len(x.api.writes), 2)
}

func TestFormattingOnlyEditToCopyIsPreserved(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	s, _ = x.ok(s)
	firstText := asObject(list(asObject(list(asObject(x.api.rows["jira"][0].Body)["content"])[0])["content"])[0])
	firstText["marks"] = []any{Object{"type": "strong"}}
	_, entries := x.ok(s)
	eq(t, entries[0].Kind, "blocked")
	x.api.rows["github"][0].Body = "Edited original"
	_, entries = x.ok(s)
	eq(t, entries[0].Kind, "blocked")
	eq(t, len(x.api.writes), 2)
}

func TestDeletedOriginalDoesNotTurnCopyIntoSource(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	s, _ = x.ok(s)
	x.api.rows["github"] = nil
	_, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{"unavailable"})
	eq(t, len(x.api.writes), 2)
}

func TestSourceEditedBeforePendingCopyRecovered(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	x.api.loseResponse = true
	x.fails(s)
	pending := x.last()
	x.api.rows["github"][0].Body = "Edited before recovery"
	x.api.loseResponse = false
	s, _ = x.ok(pending)
	x.ok(s)
	eq(t, lastWrite(x.api), write{"edit", "jira"})
	eq(t, len(x.api.rows["jira"]), 1)
}

func TestDeletionsNeverPropagateOrRecreate(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	s, _ = x.ok(s)
	x.api.rows["jira"] = nil
	_, entries := x.ok(s)
	eq(t, entries[0].Kind, "unavailable")
	eq(t, len(x.api.writes), 2)
}

func TestCommentLostResponseRecoversWithoutDuplicate(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	x.api.loseResponse = true
	x.fails(s)
	pending := x.last()
	x.api.loseResponse = false
	s, entries := x.ok(pending)
	eq(t, entries[0].Kind, "recover_comment")
	eq(t, len(x.api.writes), 2)
	x.ok(s)
	eq(t, len(x.api.writes), 2)
}

func TestEditLostResponseConvergesWithoutSecondWrite(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	s, _ = x.ok(s)
	x.api.rows["github"][0].Body = "Edited"
	x.api.loseResponse = true
	x.fails(s)
	x.api.loseResponse = false
	x.ok(s)
	eq(t, len(x.api.writes), 3)
}

func TestPendingCommentWithoutVisibleCopyBlocks(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	x.api.loseResponse = true
	x.fails(s)
	x.api.rows["jira"] = nil
	_, entries := x.ok(x.last())
	eq(t, entries[0].Kind, "blocked")
	eq(t, len(x.api.writes), 2)
}

func TestRestrictedJiraCommentsNeverExport(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	raw := jiraComment(22, "private")
	raw.Restricted = true
	x.api.rows["jira"] = []Comment{raw}
	_, entries := x.ok(s)
	eq(t, entries[0].Kind, "excluded")
	eq(t, len(x.api.writes), 1)
}

func TestUnsupportedJiraContentBlocksWithoutLoss(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	raw := jiraComment(22, "")
	asObject(raw.Body)["content"] = []any{Object{"type": "mediaSingle"}}
	x.api.rows["jira"] = []Comment{raw}
	_, entries := x.ok(s)
	eq(t, entries[0].Kind, "blocked")
}

func TestBudgetDefersExcessWritesToLaterRuns(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.c.MaxOperations = 1
	x.api.rows["github"] = []Comment{ghComment(11, "one"), ghComment(12, "two")}
	s, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{"create_comment", "deferred"})
	eq(t, len(x.api.writes), 2)
	s, entries = x.ok(s)
	eq(t, kinds(entries), []Kind{"noop", "create_comment"})
	eq(t, len(x.api.writes), 3)
	eq(t, len(s.Comments), 2)
}

func TestUnchangedCommentsAreNoopsWithoutRereads(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "GitHub original")}
	x.api.rows["jira"] = []Comment{jiraComment(22, "Jira original")}
	s, _ = x.ok(s)
	x.api.reads = nil
	_, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{"noop", "noop"})
	eq(t, len(x.api.reads), 0)
}

func TestDeletedGitHubIssueReportedWithoutStallingOtherPairs(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	s.Pairs["2"] = Pair{GitHub: 2, GitHubID: "43", JiraID: "201", JiraKey: "ENG-2"}
	x.api.rows["github"] = []Comment{ghComment(11, "still syncs")}
	s, entries := x.ok(s)
	eq(t, sorted(kinds(entries)), []Kind{"create_comment", "unavailable"})
	eq(t, lastWrite(x.api), write{"create", "jira"})
	if _, ok := s.Pairs["1"]; !ok {
		t.Fatal("pair 1 lost")
	}
}

func TestTransferredOrDeletedTrackedIssueIsUnavailable(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.githubGone = true
	_, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{"unavailable"})
}

func TestReplacedGitHubIssueIsUnavailable(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.source.ID = 99
	_, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{"unavailable"})
	eq(t, len(x.api.writes), 1)
}

func TestDeletedOrMovedJiraIssueIsUnavailable(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	x.api.jiraGone = true
	_, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{"unavailable"})
	eq(t, len(x.api.writes), 1)
}

func TestGitHubQuoteReplyOfCopyStillSyncs(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["jira"] = []Comment{jiraComment(22, "Jira original")}
	s, _ = x.ok(s)
	lines := strings.Split(x.api.rows["github"][0].Body.(string), "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	quoted := strings.Join(lines, "\n")
	x.api.rows["github"] = append(x.api.rows["github"],
		ghComment(31, quoted+"\n\nBottom reply"), ghComment(32, "Top reply\n\n"+quoted))
	s, entries := x.ok(s)
	eq(t, sorted(kinds(entries)), []Kind{"create_comment", "create_comment", "noop"})
	eq(t, len(s.Comments), 3)
}

func TestJiraQuoteReplyOfCopyStillSyncs(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "GitHub original")}
	s, _ = x.ok(s)
	reply := jiraComment(41, "Top reply")
	body := asObject(reply.Body)
	body["content"] = append(list(body["content"]),
		Object{"type": "blockquote", "content": asObject(x.api.rows["jira"][0].Body)["content"]})
	x.api.rows["jira"] = append(x.api.rows["jira"], reply)
	_, entries := x.ok(s)
	eq(t, sorted(kinds(entries)), []Kind{"create_comment", "noop"})
	eq(t, lastWrite(x.api), write{"create", "github"})
}

func TestRejectedIssueCreationIsRetriedOnNextRun(t *testing.T) {
	x := newSyncer(t)
	x.api.refuse = []int{400}
	s, entries := x.ok(nil)
	eq(t, entries[0].Outcome, "rejected")
	eq(t, s.Operations[entries[0].Operation].Status, "rejected")
	eq(t, len(x.api.writes), 0)
	s, entries = x.ok(s)
	eq(t, entries[0].Kind, "create_issue")
	eq(t, entries[0].PreviousRejection, 400)
	eq(t, s.Pairs["1"].JiraKey, "ENG-1")
}

func TestRejectedCommentDoesNotStopLaterWrites(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "one"), ghComment(12, "two")}
	x.api.refuse = []int{422}
	s, entries := x.ok(s)
	eq(t, []Outcome{entries[0].Outcome, entries[1].Outcome}, []Outcome{"rejected", ""})
	eq(t, len(s.Comments), 1)
	s, entries = x.ok(s)
	eq(t, sorted(kinds(entries)), []Kind{"create_comment", "noop"})
	eq(t, len(s.Comments), 2)
}

func TestAuthRejectionStopsRunButLeavesIntentRetryable(t *testing.T) {
	x := newSyncer(t)
	x.api.refuse = []int{401}
	x.fails(nil)
	for _, op := range x.last().Operations {
		eq(t, op.Status, "rejected")
	}
	s, _ := x.ok(x.last())
	if _, ok := s.Pairs["1"]; !ok {
		t.Fatal("retry did not create the issue")
	}
}

func TestRejectedEditIsPlannedAgain(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	s, _ = x.ok(s)
	x.api.rows["github"][0].Body = "Edited"
	x.api.refuse = []int{400}
	s, entries := x.ok(s)
	eq(t, entries[0].Outcome, "rejected")
	_, entries = x.ok(s)
	eq(t, entries[0].Kind, "edit_comment")
	eq(t, lastWrite(x.api), write{"edit", "jira"})
}

func TestCRLFBodiesCopyCleanlyAndStayStable(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "line one\r\nline two")}
	s, _ = x.ok(s)
	copied := jiraText(t, x.api.rows["jira"][0])
	if strings.Contains(copied, "\r") || !strings.Contains(copied, "line one\nline two") {
		t.Fatalf("copy not normalized: %q", copied)
	}
	x.api.rows["github"][0].Body = "line one\nline two"
	_, entries := x.ok(s)
	eq(t, entries[0].Kind, "noop")
}

func TestMismatchedRecoveryBlocksOnlyThatComment(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	x.api.loseResponse = true
	x.fails(s)
	pending := x.last()
	x.api.loseResponse = false
	x.api.rows["jira"][0].Body = toADF(strings.Replace(jiraText(t, x.api.rows["jira"][0]), "Original", "Tampered", 1))
	x.api.rows["github"] = append(x.api.rows["github"], ghComment(12, "Later comment"))
	_, entries := x.ok(pending)
	eq(t, kinds(entries), []Kind{"blocked", "create_comment"})
	eq(t, lastWrite(x.api), write{"create", "jira"})
}

func TestAcknowledgedEntriesStayQuietUntilReasonChanges(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	s, _ = x.ok(s)
	x.api.rows["github"] = nil
	plan := func() []*Entry {
		entries, err := Plan(x.c, s, x.api, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}
	entries := plan()
	op := entries[0].Operation
	if _, err := Acknowledge(s, entries, []string{strings.Repeat("0", 64)}); err == nil {
		t.Fatal("acknowledging an unknown operation must fail")
	}
	s, _ = Acknowledge(s, entries, []string{op})
	eq(t, entries[0].Kind, "acknowledged")
	entries = plan()
	again, _ := Acknowledge(s, entries, nil)
	eq(t, again.Acknowledged, s.Acknowledged)
	eq(t, entries[0].Kind, "acknowledged")
	x.api.rows["github"] = []Comment{ghComment(11, "Original")}
	x.api.rows["jira"][0].Restricted = true
	entries = plan()
	lapsed, _ := Acknowledge(s, entries, nil)
	eq(t, len(lapsed.Acknowledged), 0)
	eq(t, entries[0].Kind, "blocked")
}

// quietPair returns a linked pair with synced comments and issue timestamps, after one clean full scan.
func quietPair(t *testing.T, x *syncer) *State {
	t.Helper()
	s := x.linked()
	x.api.source.UpdatedAt = "2026-01-02T00:00:00Z"
	x.api.jira.Fields.Updated = "2026-01-02T00:00:00.000+0000"
	x.api.rows["github"] = []Comment{ghComment(11, "GitHub original")}
	s, _ = x.ok(s)
	s, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{"noop", "reconciled"})
	if _, ok := s.Reconciled["1"]; !ok {
		t.Fatal("pair not recorded as reconciled")
	}
	x.api.listReads = nil
	return s
}

func TestUnchangedPairSkipsCommentReads(t *testing.T) {
	x := newSyncer(t)
	s := quietPair(t, x)
	_, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{"unchanged"})
	eq(t, len(x.api.listReads), 0)
}

func TestIssueActivityTriggersFullScan(t *testing.T) {
	for _, side := range sides {
		t.Run(string(side), func(t *testing.T) {
			x := newSyncer(t)
			s := quietPair(t, x)
			if side == "github" {
				x.api.source.UpdatedAt = "2026-01-03T00:00:00Z"
			} else {
				x.api.jira.Fields.Updated = "2026-01-03T00:00:00.000+0000"
			}
			x.api.rows["jira"] = append(x.api.rows["jira"], jiraComment(22, "New Jira comment"))
			_, entries := x.ok(s)
			if !slices.Contains(kinds(entries), "create_comment") {
				t.Fatalf("no full scan: %v", kinds(entries))
			}
			eq(t, lastWrite(x.api), write{"create", "github"})
		})
	}
}

func TestPeriodicRescanCatchesEditsWithoutIssueActivity(t *testing.T) {
	x := newSyncer(t)
	s := quietPair(t, x)
	x.api.rows["github"][0].Body = "Edited without bumping issue timestamps"
	scanned, err := parseTimestamp(s.Reconciled["1"].Scanned)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := Plan(x.c, s, x.api, scanned.Add(23*time.Hour))
	eq(t, kinds(entries), []Kind{"unchanged"})
	entries, _ = Plan(x.c, s, x.api, scanned.Add(24*time.Hour))
	eq(t, kinds(entries), []Kind{"edit_comment"})
}

func TestPairsNeedingAttentionAreNeverMarkedQuiet(t *testing.T) {
	x := newSyncer(t)
	s := quietPair(t, x)
	x.api.source.UpdatedAt = "2026-01-03T00:00:00Z"
	x.api.rows["github"] = nil
	s, entries := x.ok(s)
	eq(t, kinds(entries), []Kind{"unavailable"})
	eq(t, s.Reconciled["1"].GitHub, "2026-01-02T00:00:00Z")
	_, entries = x.ok(s)
	eq(t, kinds(entries), []Kind{"unavailable"})
}

func TestPendingOperationForcesFullScan(t *testing.T) {
	x := newSyncer(t)
	s := quietPair(t, x)
	s.Operations[token(x.c, OpGitHubComment, "99")] = Operation{Kind: "comment_github", SourceID: "99", GitHub: 1, Status: "pending"}
	_, entries := x.ok(s)
	if slices.Contains(kinds(entries), "unchanged") || len(x.api.listReads) == 0 {
		t.Fatalf("pending operation did not force a scan: %v", kinds(entries))
	}
}

func TestTrailingMarkerIdentifiesOnlyRealCopies(t *testing.T) {
	op := strings.Repeat("a", 64)
	m := marker(op)
	eq(t, hasMarker(Comment{Side: SideGitHub, Body: "Synced\n\n" + m + "\n"}, op), true)
	eq(t, hasMarker(Comment{Side: SideGitHub, Body: "> " + m + "\n\nreply"}, ""), false)
	eq(t, hasMarker(Comment{Side: SideGitHub, Body: "reply\n\n> " + m}, ""), false)
	eq(t, hasMarker(Comment{Side: SideGitHub, Body: "mentions " + m + " inline"}, ""), false)
	doc := toADF("Synced\n\n" + m)
	doc["content"] = append(list(doc["content"]), Object{"type": "paragraph", "content": []any{}})
	eq(t, hasMarker(Comment{Side: SideJira, Body: doc}, op), true)
	eq(t, hasMarker(Comment{Side: SideJira, Body: doc}, strings.Repeat("b", 64)), false)
}

func TestPrewriteSourceChangeAborts(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "original")}
	entries, _ := Plan(x.c, s, x.api, time.Now())
	x.api.rows["github"][0].Body = "changed"
	if _, err := Apply(x.c, s, x.api, entries, func(*State) error { return nil }); err == nil {
		t.Fatal("expected abort")
	}
	eq(t, len(x.api.writes), 1)
}

func TestPrewriteCopyChangeAbortsEdit(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "original")}
	s, _ = x.ok(s)
	x.api.rows["github"][0].Body = "changed"
	entries, _ := Plan(x.c, s, x.api, time.Now())
	x.api.rows["jira"][0].Body = toADF("human changed copy")
	if _, err := Apply(x.c, s, x.api, entries, func(*State) error { return nil }); err == nil {
		t.Fatal("expected abort")
	}
	eq(t, len(x.api.writes), 2)
}

func TestBindingAndBadConfigFailClosed(t *testing.T) {
	c := testConfig(t, nil)
	other := testConfig(t, Object{"jira_project": "OTHER"})
	if validateState(NewState(c), other) == nil {
		t.Fatal("state bound to another project must be rejected")
	}
	base := Object{"repository": "acme/test", "jira_url": "https://acme.atlassian.net", "jira_project": "ENG", "jira_issue_type": "10001"}
	for _, bad := range []Object{{"unknown": true}, {"jira_url": "http://unsafe"}, {"selection_label": ""},
		{"jira_create_fields": Object{"project": Object{}}}, {"max_operations": true}, {"full_rescan_hours": 0},
		{"max_pages": 1.5}, {"comments_since": "2026-02-01T00:00:00"}} {
		raw := Object{}
		maps.Copy(raw, base)
		maps.Copy(raw, bad)
		if _, err := ParseConfig(canonical(raw, false)); err == nil {
			t.Errorf("config %v accepted", bad)
		}
	}
	if _, err := ParseConfig([]byte("[]")); err == nil {
		t.Error("non-object config accepted")
	}
}

func TestReportOmitsCommentBodies(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	x.api.rows["github"] = []Comment{ghComment(11, "secret comment")}
	entries, _ := Plan(x.c, s, x.api, time.Now())
	data, _ := json.Marshal(entries)
	if strings.Contains(string(data), "secret comment") {
		t.Fatal("report leaked comment text")
	}
}

func TestCommentCutoff(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	cutoff, _ := parseTimestamp("2026-02-01T00:00:00Z")
	x.c.CommentsSince = &cutoff
	x.api.rows["github"] = []Comment{ghComment(11, "old")}
	_, entries := x.ok(s)
	eq(t, entries[0].Kind, "excluded")
}

func TestStateRoundTripsAndRejectsUnknownKeys(t *testing.T) {
	x := newSyncer(t)
	s := x.linked()
	parsed, err := ParseState(canonical(s, true))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, string(canonical(parsed, false)), string(canonical(s, false)))
	if _, err := ParseState([]byte(`{"version":2,"binding":"x","pairs":{},"operations":{},"comments":{},"extra":{}}`)); err == nil {
		t.Fatal("unknown state key accepted")
	}
	legacy, err := ParseState([]byte(`{"version":2,"binding":"x","pairs":{},"operations":{},"comments":{}}`))
	if err != nil || legacy.Acknowledged == nil || legacy.Reconciled == nil {
		t.Fatalf("state without optional keys must load: %v", err)
	}
}
