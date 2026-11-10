package ghjira

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

// sideNames are how report sides read in a summary.
var sideNames = map[Side]string{SideGitHub: "GitHub", SideJira: "Jira", SideProject: "the project"}

// changeNames describe each writing kind as an action, completed by changeDetail.
var changeNames = map[Kind]string{
	KindCreateIssue:       "Create Jira issue",
	KindLinkIssue:         "Link existing Jira issue",
	KindCreateComment:     "Copy comment",
	KindRecoverComment:    "Finish copying comment",
	KindEditComment:       "Update comment copy",
	KindConfirmComment:    "Confirm comment copy",
	KindSyncStatus:        "Set status",
	KindAddToProject:      "Add to project",
	KindSyncProjectStatus: "Set project status",
	KindSyncParent:        "Set parent",
	KindCreateGitHubIssue: "Create GitHub issue",
	KindLinkGitHubIssue:   "Link existing GitHub issue",
	KindSyncContent:       "Update",
	KindSyncAssignee:      "Set assignee",
}

// reportFile is a report as written. Sides are plain strings here because a report
// may name SideProject, which Side's decoding rejects to keep it out of state.
type reportFile struct {
	report
	Entries []struct {
		*Entry
		Source      string `json:"source,omitempty"`
		Destination string `json:"destination,omitempty"`
	} `json:"entries"`
}

// summaryLinks builds links to issues from the deployment's config.
type summaryLinks struct{ github, jira string }

func (l summaryLinks) issue(e *Entry) string {
	var refs []string
	if e.GitHub != 0 {
		refs = append(refs, "["+"#"+strconv.Itoa(e.GitHub)+"]("+l.github+"/issues/"+strconv.Itoa(e.GitHub)+")")
	}
	if e.JiraKey != "" {
		refs = append(refs, "["+e.JiraKey+"]("+l.jira+"/browse/"+e.JiraKey+")")
	}

	return strings.Join(refs, " / ")
}

// Summarize prints a Markdown summary of the report at reportPath, for a GitHub
// Actions job summary. It needs only the report and the config, so it also works
// after a run that failed partway.
func Summarize(configPath, reportPath string, out io.Writer) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	c, err := ParseConfig(data)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		return fmt.Errorf("read report: %w", err)
	}
	var file reportFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return fmt.Errorf("parse report: %w", err)
	}
	rep := file.report
	for _, row := range file.Entries {
		row.Entry.Source, row.Entry.Destination = Side(row.Source), Side(row.Destination)
		rep.Entries = append(rep.Entries, row.Entry)
	}
	web := strings.TrimSuffix(c.GitHubURL, "/api/v3")
	if web == "https://api.github.com" {
		web = "https://github.com"
	}
	if _, err := io.WriteString(out, renderSummary(&rep, summaryLinks{github: web + "/" + c.Repository, jira: c.JiraURL})); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}

	return nil
}

func renderSummary(rep *report, links summaryLinks) string {
	var review, retry, rejected, changes, deferred []*Entry
	counts := map[Kind]int{}
	for _, e := range rep.Entries {
		switch {
		case e.Kind.needsReview():
			review = append(review, e)
		case e.Kind == KindDeferred:
			deferred = append(deferred, e)
		case e.Outcome == OutcomeRetry:
			retry = append(retry, e)
		case e.Outcome == OutcomeRejected:
			rejected = append(rejected, e)
		case e.Kind.writes():
			changes = append(changes, e)
		default:
			counts[e.Kind]++
		}
	}
	var b strings.Builder
	b.WriteString(summaryHeadline(rep, len(review)))
	if rep.Error != "" {
		b.WriteString("\n> [!CAUTION]\n> The run stopped: " + cell(rep.Error) + "\n")
	}
	if len(review) > 0 {
		b.WriteString("\n### Needs review\n\nFix each one, or silence it with the workflow's `acknowledge` input and its operation ID.\n\n")
		b.WriteString("| Issue | Reason | Operation ID |\n| --- | --- | --- |\n")
		for _, e := range review {
			b.WriteString("| " + links.issue(e) + " | " + cell(e.Reason) + " | `" + e.Operation + "` |\n")
		}
	}
	entryTable(&b, "Retried next run", "Changed while this run was writing; the next run plans it again.", retry, links)
	entryTable(&b, "Rejected", "Refused by the API; retried next run once the cause is fixed.", rejected, links)
	heading := "Changes made"
	if rep.Mode != "apply" {
		heading = "Planned changes (dry run)"
	} else if rep.Error != "" {
		heading = "Planned changes (the run stopped before finishing them)"
	}
	collapsed(&b, heading, "| Issue | Change |", changes, func(e *Entry) string { return links.issue(e) + " | " + cell(changeDetail(e)) })
	collapsed(&b, "Deferred to a later run", "| Issue | Reason |", deferred, func(e *Entry) string { return links.issue(e) + " | " + cell(e.Reason) })
	if len(counts) > 0 {
		b.WriteString("\n<details><summary>Everything else</summary>\n\n| Kind | Issues |\n| --- | --- |\n")
		kinds := slices.Sorted(maps.Keys(counts))
		for _, k := range kinds {
			b.WriteString("| " + strings.ReplaceAll(string(k), "_", " ") + " | " + strconv.Itoa(counts[k]) + " |\n")
		}
		b.WriteString("\n</details>\n")
	}

	return b.String()
}

// collapsed writes a two-column table of entries inside a closed <details> block,
// titled with the entry count.
func collapsed(b *strings.Builder, title, header string, entries []*Entry, row func(*Entry) string) {
	if len(entries) == 0 {
		return
	}
	b.WriteString("\n<details><summary>" + title + " (" + strconv.Itoa(len(entries)) + ")</summary>\n\n")
	b.WriteString(header + "\n| --- | --- |\n")
	for _, e := range entries {
		b.WriteString("| " + row(e) + " |\n")
	}
	b.WriteString("\n</details>\n")
}

func summaryHeadline(rep *report, review int) string {
	var status string
	switch {
	case rep.ExitCode == 1:
		status = "❌ Failed"
	case review > 0:
		status = "⚠️ Needs review (" + strconv.Itoa(review) + ")"
	default:
		status = "✅ In sync"
	}
	line := "## GitHub ↔ Jira sync: " + status + "\n\nMode: **" + rep.Mode + "**"
	if rep.PlannedWrites != nil {
		line += " · planned writes: " + strconv.Itoa(*rep.PlannedWrites)
	}
	if rep.DeferredWrites != nil && *rep.DeferredWrites > 0 {
		line += " · deferred to a later run: " + strconv.Itoa(*rep.DeferredWrites)
	}

	return line + "\n"
}

func entryTable(b *strings.Builder, title, note string, entries []*Entry, links summaryLinks) {
	if len(entries) == 0 {
		return
	}
	b.WriteString("\n### " + title + "\n\n" + note + "\n\n| Issue | Change | Reason |\n| --- | --- | --- |\n")
	for _, e := range entries {
		b.WriteString("| " + links.issue(e) + " | " + cell(changeDetail(e)) + " | " + cell(e.Reason) + " |\n")
	}
}

// changeDetail describes a planned write, such as "Set parent on Jira to #12".
func changeDetail(e *Entry) string {
	text, ok := changeNames[e.Kind]
	if !ok {
		text = strings.ReplaceAll(string(e.Kind), "_", " ")
	}
	if e.Field != "" {
		text += " " + e.Field
	}
	side, hasSide := sideNames[e.Destination]
	switch {
	case e.Kind == KindSyncProjectStatus && e.Destination == SideJira:
		text = "Set Jira status" // the project status is copied to Jira as a transition
	case hasSide && (e.Kind == KindCreateComment || e.Kind == KindRecoverComment):
		text += " to " + side
	case hasSide && e.Kind != KindSyncProjectStatus && e.Kind != KindAddToProject:
		text += " on " + side
	}
	for _, target := range []string{e.Status, e.Parent, e.Assignee} {
		if target != "" {
			text += " to " + target
		}
	}
	if e.Outcome == OutcomeAlreadyDone {
		text += " (already done)"
	}

	return text
}

// cell keeps text inside one Markdown table cell.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\r", " ", "\n", " ").Replace(s)
}
