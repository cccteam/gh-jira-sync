package ghjira

// Plan first; journal each create before dispatch; recover without blind retries.

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	writeKinds   = map[string]bool{"create_issue": true, "create_comment": true, "edit_comment": true}
	reviewKinds  = map[string]bool{"blocked": true, "unavailable": true}
	passiveKinds = map[string]bool{"blocked": true, "noop": true, "excluded": true, "unavailable": true,
		"deferred": true, "acknowledged": true, "unchanged": true}
	// 4xx answers about one item; anything else (auth, permissions, rate limits) affects every write.
	itemRejections = map[int]bool{400: true, 404: true, 409: true, 413: true, 422: true}
	// Fields that drive sync decisions; reactions or counters changing mid-run do not matter.
	relevantFields = []string{"id", "number", "title", "body", "html_url", "user", "author", "created",
		"created_at", "visibility", "jsdPublic", "properties"}
	sides = []string{"github", "jira"}
)

const textLimit = 30000

// Entry is one planned decision. Exported fields form the report; the rest carry
// what apply needs and never reach reports (they can hold comment text).
type Entry struct {
	GitHub            int    `json:"github"`
	Operation         string `json:"operation"`
	Kind              string `json:"kind"`
	Reason            string `json:"reason,omitempty"`
	Source            string `json:"source,omitempty"`
	CommentID         string `json:"comment_id,omitempty"`
	Destination       string `json:"destination,omitempty"`
	PreviousRejection int    `json:"previous_rejection,omitempty"`
	Outcome           string `json:"outcome,omitempty"`

	source      Object // GitHub issue or source comment as planned
	pair        Pair
	link        Link
	raw         map[string]Object // both sides of a linked comment as planned
	text        string
	prefix      string
	suffix      string
	destination Object // copy found for a pending comment
	record      Operation
	stamp       Stamp
}

func (e *Entry) block(reason string) { e.Kind, e.Reason = "blocked", reason }

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
		if (r.Status != "pending" && r.Status != "done" && r.Status != "rejected") || op != token(c, r.Kind, r.SourceID) {
			return syncErr("Invalid operation journal")
		}
	}
	seen := map[string]map[string]bool{"github": {}, "jira": {}}
	for op, link := range s.Comments {
		if r, ok := s.Operations[op]; !ok || r.Status != "done" || (link.Origin != "github" && link.Origin != "jira") {
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

func issuePair(source, jira Object) Pair {
	number, _ := strconv.Atoi(idString(source["number"]))
	key, _ := jira["key"].(string)
	return Pair{GitHub: number, GitHubID: idString(source["id"]), JiraID: idString(jira["id"]), JiraKey: key}
}

func other(side string) string {
	if side == "github" {
		return "jira"
	}
	return "github"
}

// content is a comment's text without the copy envelope.
func content(side string, raw Object, link Link) (string, error) {
	text, err := commentText(side, raw)
	if err != nil || side == link.Origin {
		return text, err
	}
	if !strings.HasPrefix(text, link.Prefix) || !strings.HasSuffix(text, link.Suffix) || len(text) < len(link.Prefix)+len(link.Suffix) {
		return "", syncErr("Copied comment attribution/marker changed; review manually")
	}
	return text[len(link.Prefix) : len(text)-len(link.Suffix)], nil
}

func rendered(side, text string, link Link) string {
	if side == link.Origin {
		return text
	}
	return link.Prefix + text + link.Suffix
}

func relevant(raw Object) any {
	if raw == nil {
		return nil
	}
	out := Object{}
	for _, k := range relevantFields {
		out[k] = raw[k]
	}
	return out
}

func retryNote(e *Entry, old *Operation) {
	if old != nil && old.Status == "rejected" {
		e.PreviousRejection = old.HTTPStatus
	}
}

func envelope(c *Config, side string, raw Object, pair Pair, op string) (string, string) {
	var author, link string
	if side == "github" {
		author, _ = asObject(raw["user"])["login"].(string)
		link = idString(raw["html_url"])
	} else {
		author, _ = asObject(raw["author"])["displayName"].(string)
		link = c.JiraURL + "/browse/" + pair.JiraKey + "?focusedCommentId=" + idString(raw["id"])
	}
	if author == "" {
		author = "unknown"
	}
	prefix := "Synced from " + side + " — " + author + " (" + createdAt(raw) + ")\nSource: " + link + "\n\n"
	return prefix, "\n\n" + marker(op)
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
		if r.Status != "done" && r.GitHub == number {
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
	var entries []*Entry
	visited := map[string]bool{}
	listed, err := api.Issues()
	if err != nil {
		return nil, err
	}
	// Selected issues in API order, then tracked pairs not selected, by number.
	var order []string
	issues := map[string]Object{}
	for _, i := range listed {
		n := idString(i["number"])
		if _, dup := issues[n]; !dup {
			order = append(order, n)
		}
		issues[n] = i
	}
	tracked := sortedKeys(s.Pairs)
	sort.SliceStable(tracked, func(a, b int) bool { return len(tracked[a]) < len(tracked[b]) })
	for _, n := range tracked {
		if _, ok := issues[n]; ok {
			continue
		}
		number, _ := strconv.Atoi(n)
		issue, err := api.Issue(number)
		if err != nil {
			return nil, err
		}
		order = append(order, n)
		issues[n] = issue
	}
	for _, n := range order {
		source := issues[n]
		if _, pr := source["pull_request"]; source != nil && pr {
			continue
		}
		number, _ := strconv.Atoi(n)
		pair, tracked := s.Pairs[n]
		if !tracked {
			e, err := planIssue(c, s, api, number, source)
			if err != nil {
				return nil, err
			}
			visited[e.Operation] = true
			// Newly created/adopted pairs receive comments on the next run.
			entries = append(entries, e)
			continue
		}
		issueOp := token(c, "issue", pair.GitHubID)
		// One vanished pair is reported for review; it must not stall every other pair.
		var jira Object
		if source != nil && idString(source["id"]) == pair.GitHubID {
			if jira, err = api.JiraIssue(pair.JiraID); err != nil {
				return nil, err
			}
		}
		if jira == nil || idString(jira["id"]) != pair.JiraID {
			entries = append(entries, &Entry{GitHub: number, Operation: issueOp, Kind: "unavailable",
				Reason: "Linked issue deleted, transferred, moved, or inaccessible; comment sync paused"})
			continue
		}
		if !c.Comments {
			continue
		}
		stamp := Stamp{}
		stamp.GitHub, _ = source["updated_at"].(string)
		stamp.Jira, _ = asObject(jira["fields"])["updated"].(string)
		if quiet(c, s, n, stamp, now) {
			entries = append(entries, &Entry{GitHub: number, Operation: issueOp, Kind: "unchanged",
				Reason: "No issue activity since full scan at " + s.Reconciled[n].Scanned})
			continue
		}
		pairEntries, err := planComments(c, s, api, number, pair, visited)
		if err != nil {
			return nil, err
		}
		entries = append(entries, pairEntries...)
		// A pair with nothing to do or review may skip comment reads until its issues change.
		clean := stamp.GitHub != "" && stamp.Jira != ""
		for _, e := range pairEntries {
			clean = clean && (e.Kind == "noop" || e.Kind == "excluded")
		}
		if clean {
			stamp.Scanned = now.UTC().Format("2006-01-02T15:04:05.000000-07:00")
			entries = append(entries, &Entry{GitHub: number, Operation: issueOp, Kind: "reconciled", stamp: stamp})
		}
	}
	for _, op := range sortedKeys(s.Operations) {
		if r := s.Operations[op]; r.Status == "pending" && !visited[op] {
			entries = append(entries, &Entry{Kind: "blocked", Operation: op, GitHub: r.GitHub,
				Reason: "Pending operation source absent or excluded; review journal"})
		}
	}
	// Writes past the budget wait for a later pass, so a large backfill still makes progress.
	budget := c.MaxOperations
	for _, e := range entries {
		if writeKinds[e.Kind] {
			if budget > 0 {
				budget--
			} else {
				e.Kind, e.Reason = "deferred", "Over max_operations="+strconv.Itoa(c.MaxOperations)+"; planned again next run"
			}
		}
	}
	return entries, nil
}

func planIssue(c *Config, s *State, api API, number int, source Object) (*Entry, error) {
	op := token(c, "issue", source["id"])
	e := &Entry{GitHub: number, Operation: op, source: source}
	existing, err := api.FindIssue(source)
	if err != nil {
		return nil, err
	}
	old, journaled := s.Operations[op]
	title, _ := source["title"].(string)
	switch {
	case existing != nil:
		e.Kind, e.pair = "link_issue", issuePair(source, existing)
	case journaled && old.Status != "rejected":
		e.block("Issue creation outcome unknown or destination missing; never retry automatically")
	case runeLen(title) > 255:
		e.block("GitHub title exceeds Jira summary limit")
	case runeLen("Source: "+idString(source["html_url"])+"\n\n"+normalized(source["body"])) > textLimit:
		e.block("Issue description exceeds conservative 30000 character limit")
	default:
		e.Kind = "create_issue"
		if journaled {
			retryNote(e, &old)
		}
	}
	return e, nil
}

func planComments(c *Config, s *State, api API, number int, pair Pair, visited map[string]bool) ([]*Entry, error) {
	var entries []*Entry
	lists := map[string][]Object{}
	byID := map[string]map[string]Object{}
	for _, side := range sides {
		rows, err := api.Comments(side, pair)
		if err != nil {
			return nil, err
		}
		lists[side] = rows
		byID[side] = map[string]Object{}
		for _, row := range rows {
			byID[side][idString(row["id"])] = row
		}
	}
	paired := map[string]map[string]bool{"github": {}, "jira": {}}
	for _, op := range sortedKeys(s.Comments) {
		link := s.Comments[op]
		if link.GitHub != number {
			continue
		}
		visited[op] = true
		for _, side := range sides {
			paired[side][link.id(side)] = true
		}
		e := &Entry{GitHub: number, Operation: op, Kind: "noop", pair: pair, link: link}
		entries = append(entries, e)
		if err := planLinked(e, link, byID); err != nil {
			e.block(err.Error())
		}
	}
	for _, side := range sides {
		dest := other(side)
		for _, raw := range lists[side] {
			id := idString(raw["id"])
			if paired[side][id] || hasMarker(side, raw, "") {
				continue
			}
			op := token(c, "comment_"+side, raw["id"])
			visited[op] = true
			e := &Entry{GitHub: number, Operation: op, Source: side, CommentID: id, Destination: dest,
				source: raw, pair: pair}
			entries = append(entries, e)
			if err := planNewComment(c, s, e, lists[dest]); err != nil {
				e.block(err.Error())
			}
		}
	}
	return entries, nil
}

// planLinked decides what a tracked comment pair needs. Errors block only this entry.
func planLinked(e *Entry, link Link, byID map[string]map[string]Object) error {
	raw := map[string]Object{}
	for _, side := range sides {
		row, ok := byID[side][link.id(side)]
		if !ok {
			e.Kind, e.Reason = "unavailable", "Comment removed or inaccessible; no deletion or recreation"
			return nil
		}
		raw[side] = row
	}
	if restricted(raw["jira"]) {
		e.block("Jira comment restricted; no export or further edits")
		return nil
	}
	text, hashes := map[string]string{}, map[string]string{}
	for _, side := range sides {
		t, err := content(side, raw[side], link)
		if err != nil {
			return err
		}
		text[side], hashes[side] = t, digest(t)
	}
	origin := link.Origin
	dest := other(origin)
	expectedText := rendered(dest, text[origin], link)
	var expectedBody any = expectedText
	if dest == "jira" {
		expectedBody = toADF(expectedText)
	}
	// Raw body fingerprints protect formatting edits to copied comments.
	changedCopy := digest(raw[dest]["body"]) != link.CopyHash
	same := hashes["github"] == hashes["jira"]
	recoveredEdit := same && hashes[origin] != link.Baseline && sameJSON(raw[dest]["body"], expectedBody)
	switch {
	case changedCopy && !recoveredEdit:
		e.block("Copied comment edited; preserve it for manual review")
	case same:
		// Steady state stays a noop; only recovered edits need a confirming re-read.
		if changedCopy || hashes[origin] != link.Baseline {
			e.Kind, e.raw = "confirm_comment", raw
		}
	case hashes[dest] != link.Baseline:
		e.block("Copied comment edited; preserve it for manual review")
	case runeLen(expectedText) > textLimit:
		e.block("Comment exceeds conservative 30000 character limit")
	default:
		e.Kind, e.Destination, e.raw, e.text = "edit_comment", dest, raw, text[origin]
	}
	return nil
}

// planNewComment decides what an untracked original comment needs. Errors block only this entry.
func planNewComment(c *Config, s *State, e *Entry, destRows []Object) error {
	var matches []Object
	for _, row := range destRows {
		if hasMarker(e.Destination, row, e.Operation) {
			matches = append(matches, row)
		}
	}
	old, journaled := s.Operations[e.Operation]
	pending := journaled && old.Status == "pending"
	switch {
	case len(matches) > 1:
		e.block("Duplicate comment markers; review manually")
	case journaled && old.Status == "done":
		return syncErr("Completed comment operation has no mapping")
	case len(matches) == 1 && !pending:
		e.block("Copy exists without a pending journal entry; restore state or review")
	case len(matches) == 1:
		// Check here so a mismatched copy blocks only this entry, not the run.
		if _, err := recoveredLink(e, old, matches[0]); err != nil {
			return err
		}
		e.Kind, e.destination, e.record = "recover_comment", matches[0], old
	case pending:
		e.block("Comment creation outcome unknown; never retry automatically")
	case e.Source == "jira" && restricted(e.source):
		e.Kind, e.Reason = "excluded", "Restricted Jira comment"
	default:
		eligible, err := eligibleComment(c, e.source)
		if err != nil {
			return err
		}
		if !eligible {
			e.Kind, e.Reason = "excluded", "Before comments_since"
			return nil
		}
		text, err := commentText(e.Source, e.source)
		if err != nil {
			return err
		}
		prefix, suffix := envelope(c, e.Source, e.source, e.pair, e.Operation)
		if runeLen(prefix+text+suffix) > textLimit {
			return syncErr("Comment exceeds conservative 30000 character limit")
		}
		e.Kind, e.text, e.prefix, e.suffix = "create_comment", text, prefix, suffix
		if journaled {
			retryNote(e, &old)
		}
	}
	return nil
}

func recoveredLink(e *Entry, record Operation, destination Object) (Link, error) {
	link := Link{GitHub: e.GitHub, Origin: e.Source, Prefix: record.Prefix, Suffix: record.Suffix,
		Baseline: record.Baseline, CopyHash: digest(destination["body"])}
	setID := func(side, id string) {
		if side == "github" {
			link.GitHubID = id
		} else {
			link.JiraID = id
		}
	}
	setID(e.Source, record.SourceID)
	setID(e.Destination, idString(destination["id"]))
	text, err := content(e.Destination, destination, link)
	if err != nil {
		return Link{}, err
	}
	if digest(text) != record.Baseline {
		return Link{}, syncErr("Recovered copy content differs; resolve pending operation manually")
	}
	return link, nil
}

func finishComment(s *State, e *Entry, record Operation, destination Object) error {
	link, err := recoveredLink(e, record, destination)
	if err != nil {
		return err
	}
	s.Comments[e.Operation] = link
	record.Status = "done"
	s.Operations[e.Operation] = record
	return nil
}

// reject records a refused write: a 4xx proves nothing was created, so the intent
// becomes retryable instead of blocking forever.
func reject(s *State, e *Entry, record Operation, status int, cause error, save func(*State) error) error {
	record.Status, record.HTTPStatus = "rejected", status
	s.Operations[e.Operation] = record
	if err := save(s); err != nil {
		return err
	}
	e.Outcome = "rejected"
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
		if reviewKinds[e.Kind] {
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
			e.Kind = "acknowledged"
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
		if passiveKinds[e.Kind] {
			continue
		}
		var err error
		switch e.Kind {
		case "reconciled":
			// Saved with the final checkpoint; losing it only costs one extra scan.
			s.Reconciled[strconv.Itoa(e.GitHub)] = e.stamp
		case "create_issue", "link_issue":
			err = applyIssue(s, api, e, save)
		case "create_comment", "recover_comment":
			err = applyNewComment(s, api, e, save)
		case "edit_comment", "confirm_comment":
			err = applyLinked(s, api, e, save)
		}
		if err != nil {
			return s, err
		}
	}
	return s, save(s)
}

func applyIssue(s *State, api API, e *Entry, save func(*State) error) error {
	source, err := api.Issue(e.GitHub)
	if err != nil {
		return err
	}
	if !sameJSON(relevant(source), relevant(e.source)) {
		return syncErr("GitHub issue changed after plan; rerun")
	}
	existing, err := api.FindIssue(source)
	if err != nil {
		return err
	}
	record := Operation{Kind: "issue", SourceID: idString(source["id"]), GitHub: e.GitHub, Status: "pending"}
	if e.Kind == "link_issue" {
		if existing == nil || idString(existing["id"]) != e.pair.JiraID {
			return syncErr("Jira identity changed after plan")
		}
	} else if existing == nil {
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
	record.Status = "done"
	s.Operations[e.Operation] = record
	return save(s)
}

func applyNewComment(s *State, api API, e *Entry, save func(*State) error) error {
	rows, err := api.Comments(e.Destination, e.pair)
	if err != nil {
		return err
	}
	var copies []Object
	for _, row := range rows {
		if hasMarker(e.Destination, row, e.Operation) {
			copies = append(copies, row)
		}
	}
	if e.Kind == "recover_comment" {
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
	raw, err := api.Comment(e.Source, e.pair, e.CommentID)
	if err != nil {
		return err
	}
	if !sameJSON(relevant(raw), relevant(e.source)) {
		return syncErr("Source comment changed after plan; rerun")
	}
	record := Operation{Kind: "comment_" + e.Source, SourceID: e.CommentID, GitHub: e.GitHub, Status: "pending",
		Prefix: e.prefix, Suffix: e.suffix, Baseline: digest(e.text)}
	s.Operations[e.Operation] = record
	if err := save(s); err != nil {
		return err
	}
	result, err := api.CreateComment(e.Destination, e.pair, e.prefix+e.text+e.suffix)
	if status, refused := refusedStatus(err); refused {
		return reject(s, e, record, status, err, save)
	}
	if err != nil {
		return err
	}
	if result == nil {
		return syncErr("Invalid API response; write outcome may be unknown")
	}
	// Read back the individual comment; never assume a POST response is complete.
	if result, err = api.Comment(e.Destination, e.pair, idString(result["id"])); err != nil {
		return err
	}
	if err := finishComment(s, e, record, result); err != nil {
		return err
	}
	return save(s)
}

func applyLinked(s *State, api API, e *Entry, save func(*State) error) error {
	link := e.link
	read := func() (map[string]Object, error) {
		raw := map[string]Object{}
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
		if !sameJSON(relevant(raw[side]), relevant(e.raw[side])) {
			return syncErr("Comment changed after plan; rerun")
		}
	}
	if e.Kind == "edit_comment" {
		err := api.EditComment(e.Destination, e.pair, link.id(e.Destination), rendered(e.Destination, e.text, link))
		if status, refused := refusedStatus(err); refused && itemRejections[status] {
			// Edits are not journaled; a refused edit simply stays planned for next run.
			e.Outcome, e.Reason = "rejected", "Edit rejected with HTTP "+strconv.Itoa(status)+"; retried next run"
			return nil
		}
		if err != nil {
			return err
		}
		if raw, err = read(); err != nil {
			return err
		}
	}
	if restricted(raw["jira"]) {
		return syncErr("Jira comment visibility changed")
	}
	g, err := content("github", raw["github"], link)
	if err != nil {
		return err
	}
	j, err := content("jira", raw["jira"], link)
	if err != nil {
		return err
	}
	if g != j {
		return syncErr("Comment edit not confirmed; baseline unchanged")
	}
	stored := s.Comments[e.Operation]
	stored.Baseline = digest(g)
	stored.CopyHash = digest(raw[other(link.Origin)]["body"])
	s.Comments[e.Operation] = stored
	return save(s)
}

// isSyncError separates expected, reportable failures from unexpected ones.
func isSyncError(err error) bool {
	var s *SyncError
	var h *HTTPStatusError
	return errors.As(err, &s) || errors.As(err, &h)
}
