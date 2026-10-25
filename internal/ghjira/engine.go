package ghjira

// Plan first; journal each create before dispatch; recover without blind retries.

import (
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 4xx answers about one item; anything else (auth, permissions, rate limits) affects every write.
var itemRejections = map[int]bool{400: true, 404: true, 409: true, 413: true, 422: true}

const textLimit = 30000

// Entry is one planned decision. Exported fields form the report; the rest carry
// what apply needs and never reach reports (they can hold comment text).
type Entry struct {
	GitHub            int     `json:"github"`
	Operation         string  `json:"operation"`
	Kind              Kind    `json:"kind"`
	Reason            string  `json:"reason,omitempty"`
	Source            Side    `json:"source,omitempty"`
	CommentID         string  `json:"comment_id,omitempty"`
	Destination       Side    `json:"destination,omitempty"`
	PreviousRejection int     `json:"previous_rejection,omitempty"`
	Outcome           Outcome `json:"outcome,omitempty"`
	Status            string  `json:"status,omitempty"` // target "open" or "closed" for status entries

	issue       *GitHubIssue     // source issue as planned
	comment     Comment          // source comment as planned
	pair        Pair             // pair to create, or the pair a comment belongs to
	link        Link             // tracked comment mapping
	raw         map[Side]Comment // both sides of a linked comment as planned
	text        string           // comment text to copy or apply
	prefix      string           // copy envelope
	suffix      string
	destination Comment   // copy found for a pending comment
	record      Operation // pending journal entry being recovered
	stamp       Stamp     // issue timestamps of a clean full scan
	jira        *JiraIssue
	closed      bool        // status to apply or record
	transition  string      // Jira transition ID for a status change on Jira
	item        ProjectItem // project item as planned
	option      string      // project option to apply or record
	fieldID     int64       // project status field
	optionID    string      // ID of option, for project writes
}

func (e *Entry) block(reason string) { e.Kind, e.Reason = KindBlocked, reason }

func NewState(c *Config) *State {
	s := &State{Version: 2, Binding: binding(c)}
	s.ensureMaps()
	return s
}

func validateState(s *State, c *Config) error {
	if s.Version != 2 || s.Binding != binding(c) {
		return syncErr("Invalid state version or endpoint/project binding; restore correct state")
	}
	for n := range s.Reconciled {
		if _, ok := s.Pairs[n]; !ok {
			return syncErr("Invalid reconcile stamps")
		}
	}
	jiraIDs := map[string]bool{}
	for n, p := range s.Pairs {
		number, err := strconv.Atoi(n)
		if !isDigits(n) || err != nil || p.GitHub != number || !isDigits(p.GitHubID) || !isDigits(p.JiraID) || jiraIDs[p.JiraID] {
			return syncErr("Invalid or duplicate issue mapping")
		}
		jiraIDs[p.JiraID] = true
	}
	for op, r := range s.Operations {
		if !r.Status.valid() || op != token(c, r.Kind, r.SourceID) {
			return syncErr("Invalid operation journal")
		}
	}
	seen := map[Side]map[string]bool{SideGitHub: {}, SideJira: {}}
	for op, link := range s.Comments {
		if r, ok := s.Operations[op]; !ok || r.Status != StatusDone || !link.Origin.valid() {
			return syncErr("Invalid comment mapping")
		}
		if _, ok := s.Pairs[strconv.Itoa(link.GitHub)]; !ok || len(link.Baseline) != 64 {
			return syncErr("Invalid comment baseline")
		}
		for _, side := range sides {
			id := link.id(side)
			if !isDigits(id) || seen[side][id] {
				return syncErr("Duplicate comment mapping")
			}
			seen[side][id] = true
		}
	}
	return nil
}

func issuePair(source *GitHubIssue, jira *JiraIssue) Pair {
	return Pair{GitHub: source.Number, GitHubID: source.idString(), JiraID: jira.ID, JiraKey: jira.Key}
}

// content is a comment's text without the copy envelope.
func content(comment Comment, link Link) (string, error) {
	text, err := comment.Text()
	if err != nil || comment.Side == link.Origin {
		return text, err
	}
	if !strings.HasPrefix(text, link.Prefix) || !strings.HasSuffix(text, link.Suffix) || len(text) < len(link.Prefix)+len(link.Suffix) {
		return "", syncErr("Copied comment attribution/marker changed; review manually")
	}
	return text[len(link.Prefix) : len(text)-len(link.Suffix)], nil
}

func rendered(side Side, text string, link Link) string {
	if side == link.Origin {
		return text
	}
	return link.Prefix + text + link.Suffix
}

func retryNote(e *Entry, old *Operation) {
	if old != nil && old.Status == StatusRejected {
		e.PreviousRejection = old.HTTPStatus
	}
}

// envelope returns the readable header and the suffix of a new copy. Copies show only
// who wrote the original and where; their identity is invisible: a hidden HTML
// comment ending GitHub copies, a comment property on Jira copies. Each link stores
// its own prefix and suffix, so copies made in older formats keep working.
func envelope(c *Config, comment Comment, pair Pair, op string) (string, string) {
	author := comment.Author
	if comment.Side == SideJira {
		if author == "" {
			author = "Someone"
		}
		link := c.JiraURL + "/browse/" + pair.JiraKey + "?focusedCommentId=" + comment.ID
		return "**" + markdownEscape(author) + "** wrote in [Jira](" + link + "):\n\n", "\n\n" + hiddenMarker(op)
	}
	if author == "" {
		author = "Someone"
	}
	return author + " wrote on GitHub (" + comment.HTMLURL + "):\n\n", ""
}

// markdownEscape keeps a display name from turning into formatting.
func markdownEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "<", `\<`, "`", "\\`").Replace(s)
}

// quiet reports a fully reconciled pair that shows no activity and whose periodic
// rescan is not due. Comment edits may not bump issue timestamps, so the rescan
// bounds how long one can wait.
func quiet(c *Config, s *State, n string, stamp Stamp, now time.Time) bool {
	seen, ok := s.Reconciled[n]
	if stamp.GitHub == "" || stamp.Jira == "" || !ok || seen.GitHub != stamp.GitHub || seen.Jira != stamp.Jira {
		return false
	}
	scanned, err := parseTimestamp(seen.Scanned)
	if err != nil || !scanned.Add(time.Duration(c.FullRescanHours)*time.Hour).After(now) {
		return false
	}
	number, _ := strconv.Atoi(n)
	for _, r := range s.Operations {
		if r.Status != StatusDone && r.GitHub == number {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Plan reads both services and decides every action without writing anything.
func Plan(c *Config, s *State, api API, now time.Time) ([]*Entry, error) {
	if err := validateState(s, c); err != nil {
		return nil, err
	}
	if err := api.CheckProject(); err != nil {
		return nil, err
	}
	var project *ProjectSnapshot
	if c.ProjectStatus != nil {
		var err error
		if project, err = api.Project(); err != nil {
			return nil, err
		}
		// A mapped option missing from the project is a configuration error for every pair.
		for _, m := range c.ProjectStatus.Map {
			if _, ok := project.Options[m.Project]; !ok {
				return nil, syncErr("project_status maps option %q, which the project's %s field does not have",
					m.Project, c.ProjectStatus.Field)
			}
		}
	}
	var entries []*Entry
	visited := map[string]bool{}
	listed, err := api.Issues()
	if err != nil {
		return nil, err
	}
	// Selected issues in API order, then tracked pairs not selected, by number.
	var order []string
	issues := map[string]*GitHubIssue{}
	for i := range listed {
		n := strconv.Itoa(listed[i].Number)
		if _, dup := issues[n]; !dup {
			order = append(order, n)
		}
		issues[n] = &listed[i]
	}
	tracked := sortedKeys(s.Pairs)
	sort.SliceStable(tracked, func(a, b int) bool { return len(tracked[a]) < len(tracked[b]) })
	for _, n := range tracked {
		if _, ok := issues[n]; ok {
			continue
		}
		issue, err := api.Issue(s.Pairs[n].GitHub)
		if err != nil {
			return nil, err
		}
		order = append(order, n)
		issues[n] = issue // nil when deleted or transferred
	}
	for _, n := range order {
		source := issues[n]
		if source != nil && source.PullRequest != nil {
			continue
		}
		number, _ := strconv.Atoi(n)
		pair, tracked := s.Pairs[n]
		if !tracked {
			e, err := planIssue(c, s, api, source)
			if err != nil {
				return nil, err
			}
			visited[e.Operation] = true
			// Newly created/adopted pairs receive comments on the next run.
			entries = append(entries, e)
			continue
		}
		issueOp := token(c, OpIssue, pair.GitHubID)
		// One vanished pair is reported for review; it must not stall every other pair.
		var jira *JiraIssue
		if source != nil && source.idString() == pair.GitHubID {
			if jira, err = api.JiraIssue(pair.JiraID); err != nil {
				return nil, err
			}
		}
		if jira == nil || jira.ID != pair.JiraID {
			entries = append(entries, &Entry{GitHub: number, Operation: issueOp, Kind: KindUnavailable,
				Reason: "Linked issue deleted, transferred, moved, or inaccessible; comment sync paused"})
			continue
		}
		// Status reads only the issues already fetched, so it is checked every run.
		statusEntry, err := planStatus(c, api, pair, source, jira)
		if err != nil {
			return nil, err
		}
		if statusEntry != nil {
			entries = append(entries, statusEntry)
		}
		// After open/closed, which never writes Jira, so its pre-write checks still hold.
		if project != nil {
			projectEntry, err := planProjectStatus(c, api, project, pair, source, jira)
			if err != nil {
				return nil, err
			}
			if projectEntry != nil {
				entries = append(entries, projectEntry)
			}
		}
		if !c.Comments {
			continue
		}
		stamp := Stamp{GitHub: source.UpdatedAt, Jira: jira.Fields.Updated}
		if quiet(c, s, n, stamp, now) {
			entries = append(entries, &Entry{GitHub: number, Operation: issueOp, Kind: KindUnchanged,
				Reason: "No issue activity since full scan at " + s.Reconciled[n].Scanned})
			continue
		}
		pairEntries, err := planComments(c, s, api, pair, visited)
		if err != nil {
			return nil, err
		}
		entries = append(entries, pairEntries...)
		// A pair with nothing to do or review may skip comment reads until its issues change.
		clean := stamp.GitHub != "" && stamp.Jira != ""
		for _, e := range pairEntries {
			clean = clean && (e.Kind == KindNoop || e.Kind == KindExcluded)
		}
		if clean {
			stamp.Scanned = now.UTC().Format("2006-01-02T15:04:05.000000-07:00")
			entries = append(entries, &Entry{GitHub: number, Operation: issueOp, Kind: KindReconciled, stamp: stamp})
		}
	}
	for _, op := range sortedKeys(s.Operations) {
		if r := s.Operations[op]; r.Status == StatusPending && !visited[op] {
			entries = append(entries, &Entry{Kind: KindBlocked, Operation: op, GitHub: r.GitHub,
				Reason: "Pending operation source absent or excluded; review journal"})
		}
	}
	// Writes past the budget wait for a later pass, so a large backfill still makes progress.
	budget := c.MaxOperations
	for _, e := range entries {
		if e.Kind.writes() {
			if budget > 0 {
				budget--
			} else {
				e.Kind, e.Reason = KindDeferred, "Over max_operations="+strconv.Itoa(c.MaxOperations)+"; planned again next run"
			}
		}
	}
	return entries, nil
}

func planIssue(c *Config, s *State, api API, source *GitHubIssue) (*Entry, error) {
	op := token(c, OpIssue, source.idString())
	e := &Entry{GitHub: source.Number, Operation: op, issue: source}
	old, journaled := s.Operations[op]
	// Only a pending intent may have created an issue; state is authoritative otherwise.
	var existing *JiraIssue
	if journaled && old.Status == StatusPending {
		var err error
		if existing, err = api.FindIssue(source, old.Started); err != nil {
			return nil, err
		}
	}
	switch {
	case existing != nil:
		e.Kind, e.pair = KindLinkIssue, issuePair(source, existing)
	case journaled && old.Status != StatusRejected:
		e.block("Issue creation outcome unknown or destination missing; never retry automatically")
	case runeLen(source.Title) > 255:
		e.block("GitHub title exceeds Jira summary limit")
	case runeLen(issueDescription(source)) > textLimit:
		e.block("Issue description exceeds conservative 30000 character limit")
	default:
		e.Kind = KindCreateIssue
		if journaled {
			retryNote(e, &old)
		}
	}
	return e, nil
}

func statusName(closed bool) string {
	if closed {
		return "closed"
	}
	return "open"
}

// planStatus mirrors closing and reopening. The pair's recorded status is what both
// sides last agreed on, so whichever side now differs from it is the side that
// changed. With two states, sides that both changed agree again; only a pair with no
// recorded status that disagrees is ambiguous, and it is blocked for review.
//
// In project status mode the project drives Jira, so only Jira's side is mirrored
// here: a GitHub close or reopen waits until the project's own automation moves
// its status and project sync carries that to Jira.
func planStatus(c *Config, api API, pair Pair, source *GitHubIssue, jira *JiraIssue) (*Entry, error) {
	if c.StatusSync == nil && c.ProjectStatus == nil {
		return nil, nil
	}
	gh, jr := source.closed(), jira.done()
	e := &Entry{GitHub: pair.GitHub, Operation: token(c, OpStatus, pair.GitHubID), pair: pair, issue: source, jira: jira}
	switch {
	case gh == jr:
		if pair.Closed != nil && *pair.Closed == gh {
			return nil, nil
		}
		e.Kind, e.closed, e.Status = KindStatusAgreed, gh, statusName(gh)
	case pair.Closed == nil:
		e.block("GitHub is " + statusName(gh) + " but Jira status " + jira.Fields.Status.Name +
			" is " + statusName(jr) + ", with no agreed status recorded; align them manually")
	case *pair.Closed == gh: // Jira moved
		e.Kind, e.Destination, e.closed, e.Status = KindSyncStatus, SideGitHub, jr, statusName(jr)
	case c.StatusSync == nil: // GitHub moved, in project status mode
		return nil, nil
	default: // GitHub moved
		name := c.StatusSync.ReopenTransition
		if gh {
			name = c.StatusSync.CloseTransition
		}
		transitions, err := api.JiraTransitions(pair.JiraID)
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(transitions, func(t JiraTransition) bool { return t.Name == name })
		switch {
		case i < 0:
			e.block("Jira transition " + strconv.Quote(name) + " is not available from status " + jira.Fields.Status.Name)
		case transitions[i].To.done() != gh:
			e.block("Jira transition " + strconv.Quote(name) + " leads to " + transitions[i].To.Name +
				", which would leave the issue " + statusName(!gh))
		default:
			e.Kind, e.Destination, e.closed, e.Status = KindSyncStatus, SideJira, gh, statusName(gh)
			e.transition = transitions[i].ID
		}
	}
	return e, nil
}

// planProjectStatus mirrors the project's status field and Jira's workflow status
// through the configured map. Like open/closed, the pair records the option both
// sides last agreed on, and the side that differs from it changed. With several
// options both sides can change to different values; that, and disagreement with
// nothing recorded, block for review. An unset project status is never a change:
// it is filled from Jira.
func planProjectStatus(c *Config, api API, project *ProjectSnapshot, pair Pair, source *GitHubIssue, jira *JiraIssue) (*Entry, error) {
	ps := c.ProjectStatus
	e := &Entry{GitHub: pair.GitHub, Operation: token(c, OpProjectStatus, pair.GitHubID), pair: pair,
		issue: source, jira: jira, fieldID: project.FieldID}
	item, inProject := project.Items[source.ID]
	switch {
	case !inProject && ps.AddToProject:
		e.Kind, e.Destination = KindAddToProject, SideProject
		return e, nil
	case !inProject:
		e.Kind, e.Reason = KindExcluded, "Issue is not in the project"
		return e, nil
	case item.Archived:
		e.Kind, e.Reason = KindExcluded, "Project item is archived"
		return e, nil
	}
	e.item = item
	current := project.optionName(item.OptionID)
	jiraStatus := jira.Fields.Status.Name
	fromJira, mapped := ps.forJira(jiraStatus)
	if !mapped {
		e.block("Jira status " + strconv.Quote(jiraStatus) + " is not mapped in project_status")
		return e, nil
	}
	if _, ok := ps.mapping(current); current != "" && !ok {
		e.block("Project status " + strconv.Quote(current) + " is not mapped in project_status")
		return e, nil
	}
	setProject := func() {
		e.Kind, e.Destination, e.option, e.Status = KindSyncProjectStatus, SideProject, fromJira, fromJira
		e.optionID = project.Options[fromJira]
	}
	agreed := pair.ProjectStatus
	switch {
	case current == fromJira:
		if agreed != nil && *agreed == current {
			return nil, nil
		}
		e.Kind, e.option, e.Status = KindProjectStatusAgreed, current, current
	case current == "":
		setProject()
	case agreed == nil:
		e.block("Project status " + strconv.Quote(current) + " and Jira status " + strconv.Quote(jiraStatus) +
			" disagree, with no agreed status recorded; align them manually")
	case current == *agreed: // Jira moved
		setProject()
	case fromJira == *agreed: // the project moved
		m, _ := ps.mapping(current)
		transitions, err := api.JiraTransitions(pair.JiraID)
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(transitions, func(t JiraTransition) bool {
			if m.Transition != "" {
				return t.Name == m.Transition
			}
			return t.To.Name == m.Jira[0]
		})
		switch {
		case i < 0:
			e.block("No Jira transition from " + strconv.Quote(jiraStatus) + " to " + strconv.Quote(m.Jira[0]))
		case !slices.Contains(m.Jira, transitions[i].To.Name):
			e.block("Jira transition " + strconv.Quote(transitions[i].Name) + " leads to " +
				strconv.Quote(transitions[i].To.Name) + ", which is not mapped to " + strconv.Quote(current))
		default:
			e.Kind, e.Destination, e.option, e.Status = KindSyncProjectStatus, SideJira, current, current
			e.transition = transitions[i].ID
		}
	default:
		e.block("Both sides changed since the agreed status " + strconv.Quote(*agreed) + ": project is " +
			strconv.Quote(current) + ", Jira is " + strconv.Quote(jiraStatus) + "; align them manually")
	}
	return e, nil
}

func planComments(c *Config, s *State, api API, pair Pair, visited map[string]bool) ([]*Entry, error) {
	var entries []*Entry
	lists := map[Side][]Comment{}
	byID := map[Side]map[string]Comment{}
	for _, side := range sides {
		rows, err := api.Comments(side, pair)
		if err != nil {
			return nil, err
		}
		lists[side] = rows
		byID[side] = map[string]Comment{}
		for _, row := range rows {
			byID[side][row.ID] = row
		}
	}
	paired := map[Side]map[string]bool{SideGitHub: {}, SideJira: {}}
	for _, op := range sortedKeys(s.Comments) {
		link := s.Comments[op]
		if link.GitHub != pair.GitHub {
			continue
		}
		visited[op] = true
		for _, side := range sides {
			paired[side][link.id(side)] = true
		}
		e := &Entry{GitHub: pair.GitHub, Operation: op, Kind: KindNoop, pair: pair, link: link}
		entries = append(entries, e)
		if err := planLinked(e, link, byID); err != nil {
			e.block(err.Error())
		}
	}
	for _, side := range sides {
		for _, row := range lists[side] {
			if paired[side][row.ID] || hasMarker(row, "") {
				continue
			}
			op := token(c, commentOp(side), row.ID)
			visited[op] = true
			e := &Entry{GitHub: pair.GitHub, Operation: op, Source: side, CommentID: row.ID, Destination: side.Other(),
				comment: row, pair: pair}
			entries = append(entries, e)
			if err := planNewComment(c, s, e, lists[side.Other()]); err != nil {
				e.block(err.Error())
			}
		}
	}
	return entries, nil
}

// planLinked decides what a tracked comment pair needs. Errors block only this entry.
func planLinked(e *Entry, link Link, byID map[Side]map[string]Comment) error {
	raw := map[Side]Comment{}
	for _, side := range sides {
		row, ok := byID[side][link.id(side)]
		if !ok {
			e.Kind, e.Reason = KindUnavailable, "Comment removed or inaccessible; no deletion or recreation"
			return nil
		}
		raw[side] = row
	}
	if raw[SideJira].Restricted {
		e.block("Jira comment restricted; no export or further edits")
		return nil
	}
	text, hashes := map[Side]string{}, map[Side]string{}
	for _, side := range sides {
		t, err := content(raw[side], link)
		if err != nil {
			return err
		}
		text[side], hashes[side] = t, digest(t)
	}
	origin := link.Origin
	dest := origin.Other()
	expectedText := rendered(dest, text[origin], link)
	var expectedBody any = expectedText
	if dest == SideJira {
		expectedBody = toADF(expectedText)
	}
	// Raw body fingerprints protect formatting edits to copied comments.
	changedCopy := digest(raw[dest].Body) != link.CopyHash
	same := hashes[SideGitHub] == hashes[SideJira]
	recoveredEdit := same && hashes[origin] != link.Baseline && sameJSON(raw[dest].Body, expectedBody)
	switch {
	case changedCopy && !recoveredEdit:
		e.block("Copied comment edited; preserve it for manual review")
	case same:
		// Steady state stays a noop; only recovered edits need a confirming re-read.
		if changedCopy || hashes[origin] != link.Baseline {
			e.Kind, e.raw = KindConfirmComment, raw
		}
	case hashes[dest] != link.Baseline:
		e.block("Copied comment edited; preserve it for manual review")
	case runeLen(expectedText) > textLimit:
		e.block("Comment exceeds conservative 30000 character limit")
	default:
		e.Kind, e.Destination, e.raw, e.text = KindEditComment, dest, raw, text[origin]
	}
	return nil
}

// planNewComment decides what an untracked original comment needs. Errors block only this entry.
func planNewComment(c *Config, s *State, e *Entry, destRows []Comment) error {
	var matches []Comment
	for _, row := range destRows {
		if hasMarker(row, e.Operation) {
			matches = append(matches, row)
		}
	}
	old, journaled := s.Operations[e.Operation]
	pending := journaled && old.Status == StatusPending
	switch {
	case len(matches) > 1:
		e.block("Duplicate comment markers; review manually")
	case journaled && old.Status == StatusDone:
		return syncErr("Completed comment operation has no mapping")
	case len(matches) == 1 && !pending:
		e.block("Copy exists without a pending journal entry; restore state or review")
	case len(matches) == 1:
		// Check here so a mismatched copy blocks only this entry, not the run.
		if _, err := recoveredLink(e, old, matches[0]); err != nil {
			return err
		}
		e.Kind, e.destination, e.record = KindRecoverComment, matches[0], old
	case pending:
		e.block("Comment creation outcome unknown; never retry automatically")
	case e.comment.Restricted:
		e.Kind, e.Reason = KindExcluded, "Restricted Jira comment"
	default:
		eligible, err := eligibleComment(c, e.comment)
		if err != nil {
			return err
		}
		if !eligible {
			e.Kind, e.Reason = KindExcluded, "Before comments_since"
			return nil
		}
		text, err := e.comment.Text()
		if err != nil {
			return err
		}
		prefix, suffix := envelope(c, e.comment, e.pair, e.Operation)
		if runeLen(prefix+text+suffix) > textLimit {
			return syncErr("Comment exceeds conservative 30000 character limit")
		}
		e.Kind, e.text, e.prefix, e.suffix = KindCreateComment, text, prefix, suffix
		if journaled {
			retryNote(e, &old)
		}
	}
	return nil
}

func recoveredLink(e *Entry, record Operation, destination Comment) (Link, error) {
	link := Link{GitHub: e.GitHub, Origin: e.Source, Prefix: record.Prefix, Suffix: record.Suffix,
		Baseline: record.Baseline, CopyHash: digest(destination.Body)}
	link.setID(e.Source, record.SourceID)
	link.setID(e.Destination, destination.ID)
	text, err := content(destination, link)
	if err != nil {
		return Link{}, err
	}
	if digest(text) != record.Baseline {
		return Link{}, syncErr("Recovered copy content differs; resolve pending operation manually")
	}
	return link, nil
}

func finishComment(s *State, e *Entry, record Operation, destination Comment) error {
	link, err := recoveredLink(e, record, destination)
	if err != nil {
		return err
	}
	s.Comments[e.Operation] = link
	record.Status = StatusDone
	s.Operations[e.Operation] = record
	return nil
}

// reject records a refused write: a 4xx proves nothing was created, so the intent
// becomes retryable instead of blocking forever.
func reject(s *State, e *Entry, record Operation, status int, cause error, save func(*State) error) error {
	record.Status, record.HTTPStatus = StatusRejected, status
	s.Operations[e.Operation] = record
	if err := save(s); err != nil {
		return err
	}
	e.Outcome = OutcomeRejected
	e.Reason = "Rejected with HTTP " + strconv.Itoa(status) + "; fix the cause, retried next run"
	if !itemRejections[status] {
		return cause
	}
	return nil
}

// Acknowledge marks reviewed blocked/unavailable entries; each stays quiet until its reason changes.
func Acknowledge(state *State, entries []*Entry, ops []string) (*State, error) {
	s := state.clone()
	current := map[string]*Entry{}
	for _, e := range entries {
		if e.Kind.needsReview() {
			current[e.Operation] = e
		}
	}
	acks := cloneMap(s.Acknowledged)
	for _, op := range ops {
		e, ok := current[op]
		if !ok {
			return nil, syncErr("No blocked or unavailable entry to acknowledge for operation %s", op)
		}
		acks[op] = e.Reason
	}
	s.Acknowledged = map[string]string{}
	for op, reason := range acks {
		if e, ok := current[op]; ok && e.Reason == reason {
			s.Acknowledged[op] = reason
			e.Kind = KindAcknowledged
		}
	}
	return s, nil
}

// Apply executes planned writes, re-reading each object first and checkpointing every step.
func Apply(c *Config, state *State, api API, entries []*Entry, save func(*State) error) (*State, error) {
	s := state.clone()
	// In Actions this save must reach the state branch before any API mutation.
	if err := save(s); err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Kind.passive() {
			continue
		}
		var err error
		switch e.Kind {
		case KindReconciled:
			// Saved with the final checkpoint; losing it only costs one extra scan.
			s.Reconciled[strconv.Itoa(e.GitHub)] = e.stamp
		case KindStatusAgreed:
			// Saved with the final checkpoint; losing it only means it is recorded next run.
			setAgreedStatus(s, e.GitHub, e.closed)
		case KindSyncStatus:
			err = applyStatus(s, api, e, save)
		case KindProjectStatusAgreed:
			setAgreedProjectStatus(s, e.GitHub, e.option)
		case KindAddToProject:
			// Adding is idempotent; its status is synced on the next run.
			err = api.AddProjectItem(e.GitHub)
			if status, refused := refusedStatus(err); refused && itemRejections[status] {
				e.Outcome, e.Reason = OutcomeRejected, "Adding to the project was rejected with HTTP "+strconv.Itoa(status)+"; retried next run"
				err = nil
			}
		case KindSyncProjectStatus:
			err = applyProjectStatus(c, s, api, e, save)
		case KindCreateIssue, KindLinkIssue:
			err = applyIssue(s, api, e, save)
			// A new Jira issue starts open; recording that lets a GitHub issue that was
			// already closed close its Jira issue on the next run.
			if err == nil && e.Kind == KindCreateIssue && e.Outcome == "" && (c.StatusSync != nil || c.ProjectStatus != nil) {
				setAgreedStatus(s, e.GitHub, false)
			}
		case KindCreateComment, KindRecoverComment:
			err = applyNewComment(s, api, e, save)
		case KindEditComment, KindConfirmComment:
			err = applyLinked(s, api, e, save)
		}
		if err != nil {
			return s, err
		}
	}
	return s, save(s)
}

func setAgreedStatus(s *State, number int, closed bool) {
	n := strconv.Itoa(number)
	pair := s.Pairs[n]
	pair.Closed = &closed
	s.Pairs[n] = pair
}

// applyStatus closes or reopens one side. Status changes are idempotent and not
// journaled: if a response is lost, the next run sees both sides agree and records it.
func applyStatus(s *State, api API, e *Entry, save func(*State) error) error {
	current := func() (*GitHubIssue, *JiraIssue, error) {
		source, err := api.Issue(e.GitHub)
		if err != nil {
			return nil, nil, err
		}
		jira, err := api.JiraIssue(e.pair.JiraID)
		if err != nil || source == nil || jira == nil {
			if err == nil {
				err = syncErr("Linked issue vanished after plan; rerun")
			}
			return nil, nil, err
		}
		return source, jira, nil
	}
	source, jira, err := current()
	if err != nil {
		return err
	}
	if source.closed() != e.issue.closed() || jira.done() != e.jira.done() {
		return syncErr("Issue status changed after plan; rerun")
	}
	if e.Destination == SideGitHub {
		err = api.SetGitHubState(e.GitHub, e.closed)
	} else {
		err = api.TransitionJira(e.pair.JiraID, e.transition)
	}
	if status, refused := refusedStatus(err); refused && itemRejections[status] {
		// For example a transition screen that needs fields; nothing changed, so retry next run.
		e.Outcome, e.Reason = OutcomeRejected, "Status change rejected with HTTP "+strconv.Itoa(status)+"; retried next run"
		return nil
	}
	if err != nil {
		return err
	}
	if source, jira, err = current(); err != nil {
		return err
	}
	if source.closed() != e.closed || jira.done() != e.closed {
		return syncErr("Status change not confirmed; GitHub is %s, Jira status %s is %s",
			statusName(source.closed()), jira.Fields.Status.Name, statusName(jira.done()))
	}
	setAgreedStatus(s, e.GitHub, e.closed)
	return save(s)
}

func setAgreedProjectStatus(s *State, number int, option string) {
	n := strconv.Itoa(number)
	pair := s.Pairs[n]
	pair.ProjectStatus = &option
	s.Pairs[n] = pair
}

// applyProjectStatus sets the project item or transitions Jira. Like status changes
// it is idempotent and not journaled, and both sides are checked before and after.
func applyProjectStatus(c *Config, s *State, api API, e *Entry, save func(*State) error) error {
	current := func() (ProjectItem, *JiraIssue, error) {
		item, err := api.ProjectItem(e.item.ID, e.fieldID)
		if err != nil {
			return ProjectItem{}, nil, err
		}
		jira, err := api.JiraIssue(e.pair.JiraID)
		if err == nil && jira == nil {
			err = syncErr("Linked Jira issue vanished after plan; rerun")
		}
		return item, jira, err
	}
	item, jira, err := current()
	if err != nil {
		return err
	}
	if item.OptionID != e.item.OptionID || jira.Fields.Status.Name != e.jira.Fields.Status.Name {
		return syncErr("Project or Jira status changed after plan; rerun")
	}
	if e.Destination == SideProject {
		err = api.SetProjectStatus(e.item.ID, e.fieldID, e.optionID)
	} else {
		err = api.TransitionJira(e.pair.JiraID, e.transition)
	}
	if status, refused := refusedStatus(err); refused && itemRejections[status] {
		e.Outcome, e.Reason = OutcomeRejected, "Project status change rejected with HTTP "+strconv.Itoa(status)+"; retried next run"
		return nil
	}
	if err != nil {
		return err
	}
	if item, jira, err = current(); err != nil {
		return err
	}
	fromJira, _ := c.ProjectStatus.forJira(jira.Fields.Status.Name)
	if e.Destination == SideProject && item.OptionID != e.optionID || fromJira != e.option {
		return syncErr("Project status change not confirmed; Jira status is %s", jira.Fields.Status.Name)
	}
	setAgreedProjectStatus(s, e.GitHub, e.option)
	return save(s)
}

func applyIssue(s *State, api API, e *Entry, save func(*State) error) error {
	source, err := api.Issue(e.GitHub)
	if err != nil {
		return err
	}
	if !source.sameContent(e.issue) {
		return syncErr("GitHub issue changed after plan; rerun")
	}
	record := Operation{Kind: OpIssue, SourceID: source.idString(), GitHub: e.GitHub, Status: StatusPending,
		Started: time.Now().UTC().Format(time.RFC3339)}
	var existing *JiraIssue
	if e.Kind == KindLinkIssue {
		// Re-read the found issue itself; its identity property must still match.
		var err error
		existing, err = api.JiraIssue(e.pair.JiraID)
		if err != nil {
			return err
		}
		want := IssueIdentity{Operation: e.Operation, GitHubID: source.idString()}
		if existing == nil || existing.Properties.Identity == nil || *existing.Properties.Identity != want {
			return syncErr("Jira identity changed after plan")
		}
	} else {
		s.Operations[e.Operation] = record
		if err := save(s); err != nil { // Never dispatch creation without a durable intent.
			return err
		}
		existing, err = api.CreateIssue(source)
		if status, refused := refusedStatus(err); refused {
			return reject(s, e, record, status, err, save)
		}
		if err != nil {
			return err
		}
	}
	s.Pairs[strconv.Itoa(e.GitHub)] = issuePair(source, existing)
	record.Status = StatusDone
	s.Operations[e.Operation] = record
	return save(s)
}

func applyNewComment(s *State, api API, e *Entry, save func(*State) error) error {
	rows, err := api.Comments(e.Destination, e.pair)
	if err != nil {
		return err
	}
	var copies []Comment
	for _, row := range rows {
		if hasMarker(row, e.Operation) {
			copies = append(copies, row)
		}
	}
	if e.Kind == KindRecoverComment {
		if len(copies) != 1 {
			return syncErr("Pending comment recovery changed after plan")
		}
		if err := finishComment(s, e, e.record, copies[0]); err != nil {
			return err
		}
		return save(s)
	}
	if len(copies) > 0 {
		return syncErr("Comment copy appeared after plan; rerun")
	}
	source, err := api.Comment(e.Source, e.pair, e.CommentID)
	if err != nil {
		return err
	}
	if !source.sameAs(e.comment) {
		return syncErr("Source comment changed after plan; rerun")
	}
	record := Operation{Kind: commentOp(e.Source), SourceID: e.CommentID, GitHub: e.GitHub, Status: StatusPending,
		Prefix: e.prefix, Suffix: e.suffix, Baseline: digest(e.text)}
	s.Operations[e.Operation] = record
	if err := save(s); err != nil {
		return err
	}
	created, err := api.CreateComment(e.Destination, e.pair, e.prefix+e.text+e.suffix, e.Operation)
	if status, refused := refusedStatus(err); refused {
		return reject(s, e, record, status, err, save)
	}
	if err != nil {
		return err
	}
	if created.ID == "" {
		return syncErr("Invalid API response; write outcome may be unknown")
	}
	// Read back the individual comment; never assume a POST response is complete.
	copied, err := api.Comment(e.Destination, e.pair, created.ID)
	if err != nil {
		return err
	}
	if err := finishComment(s, e, record, copied); err != nil {
		return err
	}
	return save(s)
}

func applyLinked(s *State, api API, e *Entry, save func(*State) error) error {
	link := e.link
	read := func() (map[Side]Comment, error) {
		raw := map[Side]Comment{}
		for _, side := range sides {
			row, err := api.Comment(side, e.pair, link.id(side))
			if err != nil {
				return nil, err
			}
			raw[side] = row
		}
		return raw, nil
	}
	raw, err := read()
	if err != nil {
		return err
	}
	for _, side := range sides {
		if !raw[side].sameAs(e.raw[side]) {
			return syncErr("Comment changed after plan; rerun")
		}
	}
	if e.Kind == KindEditComment {
		err := api.EditComment(e.Destination, e.pair, link.id(e.Destination), rendered(e.Destination, e.text, link))
		if status, refused := refusedStatus(err); refused && itemRejections[status] {
			// Edits are not journaled; a refused edit simply stays planned for next run.
			e.Outcome, e.Reason = OutcomeRejected, "Edit rejected with HTTP "+strconv.Itoa(status)+"; retried next run"
			return nil
		}
		if err != nil {
			return err
		}
		if raw, err = read(); err != nil {
			return err
		}
	}
	if raw[SideJira].Restricted {
		return syncErr("Jira comment visibility changed")
	}
	g, err := content(raw[SideGitHub], link)
	if err != nil {
		return err
	}
	j, err := content(raw[SideJira], link)
	if err != nil {
		return err
	}
	if g != j {
		return syncErr("Comment edit not confirmed; baseline unchanged")
	}
	stored := s.Comments[e.Operation]
	stored.Baseline = digest(g)
	stored.CopyHash = digest(raw[link.Origin.Other()].Body)
	s.Comments[e.Operation] = stored
	return save(s)
}

// isSyncError separates expected, reportable failures from unexpected ones.
func isSyncError(err error) bool {
	var s *SyncError
	var h *HTTPStatusError
	return errors.As(err, &s) || errors.As(err, &h)
}
