package ghjira

import (
	"strings"
	"testing"
	"time"
)

func TestCopiesShowNoTechnicalMarkers(t *testing.T) {
	t.Parallel()
	x := newSyncer(t)
	s := x.linked()
	x.api.rows[SideGitHub] = []Comment{ghComment(11, "GitHub original")}
	x.api.rows[SideJira] = []Comment{jiraComment(22, "Jira original")}
	s, _ = x.ok(s)

	jiraCopy := x.api.rows[SideJira][1]
	text := jiraText(t, jiraCopy)
	eq(t, text, "alice wrote on GitHub (https://github.com/acme/test/issues/1#issuecomment-11):\n\nGitHub original")
	eq(t, jiraCopy.Identity, token(x.c, OpGitHubComment, "11"))

	githubCopy, _ := x.api.rows[SideGitHub][1].Body.(string)
	op := token(x.c, OpJiraComment, "22")
	eq(t, githubCopy, "**Bob** wrote in [Jira](https://acme.atlassian.net/browse/ENG-1?focusedCommentId=22):\n\n"+
		"Jira original\n\n<!-- gh-jira-sync:"+op+" -->")
	for _, visible := range []string{text, strings.TrimSuffix(githubCopy, hiddenMarker(op))} {
		if strings.Contains(visible, "gh-jira-sync") || strings.Contains(visible, "gh-sync") {
			t.Fatalf("technical marker visible: %q", visible)
		}
	}
	// Copies are recognized, so nothing echoes back.
	writes := len(x.api.writes)
	x.ok(s)
	eq(t, len(x.api.writes), writes)
}

func TestHiddenMarkerOnlyCountsAsItsOwnLastLine(t *testing.T) {
	t.Parallel()
	op := strings.Repeat("e", 64)
	m := hiddenMarker(op)
	eq(t, hasMarker(Comment{Side: SideGitHub, Body: "copy\n\n" + m + "\n"}, op), true)
	eq(t, hasMarker(Comment{Side: SideGitHub, Body: "> copy\n> " + m + "\n\nreply"}, ""), false)
	eq(t, hasMarker(Comment{Side: SideGitHub, Body: "reply\n\n> " + m}, ""), false)
	// An HTML comment in Jira text is literal text, never a marker.
	eq(t, hasMarker(Comment{Side: SideJira, Body: toADF("copy\n\n" + m)}, ""), false)
	eq(t, hasMarker(Comment{Side: SideJira, Body: toADF("anything"), Identity: op}, op), true)
}

func TestMarkdownInAuthorNamesIsEscaped(t *testing.T) {
	t.Parallel()
	prefix, _ := envelope(fakeConfig, Comment{Side: SideJira, ID: "5", Author: "*bold* [x]"}, Pair{JiraKey: "ENG-1"}, "op")
	if !strings.HasPrefix(prefix, `**\*bold\* \[x\]** wrote in`) {
		t.Fatalf("prefix: %q", prefix)
	}
}

func TestIssueRecoverySearchesOnlyPendingIntentsByStartTime(t *testing.T) {
	t.Parallel()
	x := newSyncer(t)
	x.api.loseResponse = true
	x.fails(nil)
	eq(t, len(x.api.searches), 0) // a first creation never searches
	pending := x.last()
	started := pending.Operations[token(x.c, OpIssue, "42")].Started
	if when, err := time.Parse(time.RFC3339, started); err != nil || time.Since(when) > time.Minute {
		t.Fatalf("started not recorded: %q", started)
	}
	x.api.loseResponse = false
	s, entries := x.ok(pending)
	eq(t, entries[0].Kind, KindLinkIssue)
	eq(t, x.api.searches, []string{started})
	eq(t, s.Pairs["1"].JiraKey, "ENG-1")
	x.ok(s)
	eq(t, len(x.api.searches), 1) // tracked pairs never search
}

func TestLinkRequiresMatchingIdentityProperty(t *testing.T) {
	t.Parallel()
	x := newSyncer(t)
	x.api.loseResponse = true
	x.fails(nil)
	x.api.loseResponse = false
	entries, err := Plan(x.c, x.last(), x.api, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	x.api.jira.Properties.Identity.GitHubID = "999" // someone changed the property
	if _, err := Apply(x.c, x.last(), x.api, entries, x.save); err == nil {
		t.Fatal("linked an issue whose identity no longer matches")
	}
}
