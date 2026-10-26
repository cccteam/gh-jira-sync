package ghjira

// Named types for closed sets of values, and the GitHub/Jira API shapes the sync reads.

import (
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	// GitHub issue states, also the values of issue_state.
	stateOpen   = "open"
	stateClosed = "closed"
	// ownerUser is the project owner type for a user-owned project.
	ownerUser = "user"
	// identityKey names the Jira issue and comment property holding a copy's identity.
	identityKey = "gh-jira-sync"
	// HTTP headers set on every request.
	headerAuthorization = "Authorization"
	headerAccept        = "Accept"
)

// Side names one of the two services.
type Side string

// The two sides a pair links.
const (
	SideGitHub Side = "github"
	SideJira   Side = "jira"
)

var sides = []Side{SideGitHub, SideJira}

// SideProject marks report entries that write the GitHub Project item. It is
// report-only: state never stores it, so Side's decoding still rejects it.
const SideProject Side = "project"

// Other returns the opposite service.
func (s Side) Other() Side {
	if s == SideGitHub {
		return SideJira
	}

	return SideGitHub
}

func (s Side) valid() bool { return s == SideGitHub || s == SideJira }

// UnmarshalText rejects anything but the two known sides, so state cannot carry a typo.
func (s *Side) UnmarshalText(b []byte) error {
	if v := Side(b); v.valid() {
		*s = v

		return nil
	}

	return fmt.Errorf("invalid side %q", b)
}

// Kind is the decision planned for one report entry.
type Kind string

// The kinds of report entry; see the report documentation for each.
const (
	KindCreateIssue    Kind = "create_issue"
	KindLinkIssue      Kind = "link_issue"
	KindCreateComment  Kind = "create_comment"
	KindRecoverComment Kind = "recover_comment"
	KindEditComment    Kind = "edit_comment"
	KindConfirmComment Kind = "confirm_comment"
	KindNoop           Kind = "noop"
	KindBlocked        Kind = "blocked"
	KindUnavailable    Kind = "unavailable"
	KindExcluded       Kind = "excluded"
	KindDeferred       Kind = "deferred"
	KindAcknowledged   Kind = "acknowledged"
	KindUnchanged      Kind = "unchanged"
	KindReconciled     Kind = "reconciled"
	KindSyncStatus     Kind = "sync_status"   // close or reopen one side to match the other
	KindStatusAgreed   Kind = "status_agreed" // both sides agree; record that as the baseline
	// KindAddToProject and the next two are project status mode: a GitHub Project's
	// single-select field mirrors Jira status.
	KindAddToProject        Kind = "add_to_project"
	KindSyncProjectStatus   Kind = "sync_project_status"
	KindProjectStatusAgreed Kind = "project_status_agreed"
	// KindSyncParent and KindParentAgreed are parent sync: GitHub sub-issues mirror Jira
	// parent links (for example Epic → Story).
	KindSyncParent   Kind = "sync_parent"
	KindParentAgreed Kind = "parent_agreed"
	// KindCreateGitHubIssue and KindLinkGitHubIssue are Jira → GitHub creation
	// (jira_selection_label).
	KindCreateGitHubIssue Kind = "create_github_issue"
	KindLinkGitHubIssue   Kind = "link_github_issue"
)

// writes reports kinds that perform a remote write and count against max_operations.
func (k Kind) writes() bool {
	switch k {
	case KindCreateIssue, KindCreateComment, KindEditComment, KindSyncStatus, KindAddToProject, KindSyncProjectStatus,
		KindSyncParent, KindCreateGitHubIssue:
		return true
	default:
		return false
	}
}

// needsReview reports kinds that make the run exit 2 until resolved or acknowledged.
func (k Kind) needsReview() bool { return k == KindBlocked || k == KindUnavailable }

// passive reports kinds that apply skips entirely.
func (k Kind) passive() bool {
	switch k {
	case KindBlocked, KindNoop, KindExcluded, KindUnavailable, KindDeferred, KindAcknowledged, KindUnchanged:
		return true
	default:
		return false
	}
}

// Status is where a journaled operation stands.
type Status string

// The states of a journaled operation.
const (
	StatusPending  Status = "pending"  // intent saved; outcome unknown until confirmed
	StatusDone     Status = "done"     // destination confirmed
	StatusRejected Status = "rejected" // refused with a 4xx; nothing created, retried next run
)

func (s Status) valid() bool { return s == StatusPending || s == StatusDone || s == StatusRejected }

// UnmarshalText accepts only the known operation states.
func (s *Status) UnmarshalText(b []byte) error {
	if v := Status(b); v.valid() {
		*s = v

		return nil
	}

	return fmt.Errorf("invalid status %q", b)
}

// OpKind names what an operation creates. It is hashed into the operation token,
// so its values must never change.
type OpKind string

// The journaled operation kinds.
const (
	OpIssue         OpKind = "issue"
	OpGitHubComment OpKind = "comment_github"
	// OpJiraIssue creates a GitHub issue from a Jira issue (jira_selection_label).
	OpJiraIssue   OpKind = "jira_issue"
	OpJiraComment OpKind = "comment_jira"
)

// OpStatus identifies a pair's status decisions in reports and acknowledgements.
// Status changes are not journaled, so it never appears in state operations.
const OpStatus OpKind = "status"

// OpProjectStatus identifies a pair's project status decisions, like OpStatus.
const OpProjectStatus OpKind = "project_status"

// OpParent identifies a pair's parent decisions, like OpStatus.
const OpParent OpKind = "parent"

// commentOp is the operation kind for copying a comment that originates on side.
func commentOp(side Side) OpKind {
	if side == SideGitHub {
		return OpGitHubComment
	}

	return OpJiraComment
}

// UnmarshalText accepts only the journaled operation kinds; the report-only kinds
// (status, project status, parent) never appear in state.
func (k *OpKind) UnmarshalText(b []byte) error {
	switch v := OpKind(b); v {
	case OpIssue, OpGitHubComment, OpJiraComment, OpJiraIssue:
		*k = v

		return nil
	default:
		return fmt.Errorf("invalid operation kind %q", b)
	}
}

// Outcome records what apply did that the plan could not predict.
type Outcome string

// The outcomes apply can record on an entry.
const (
	OutcomeRejected Outcome = "rejected"
	// OutcomeAlreadyDone means the planned status change was already in place when apply
	// re-checked, so nothing was written and the agreement was recorded.
	OutcomeAlreadyDone Outcome = "already_done"
)

// GitHubIssue is the part of a GitHub issue the sync reads.
type GitHubIssue struct {
	ID      int64  `json:"id"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"` // null decodes as ""
	HTMLURL string `json:"html_url"`
	State   string `json:"state"` // "open" or "closed"
	// Type is the GitHub issue type (organization-defined, such as Bug or Feature).
	Type *struct {
		Name string `json:"name"`
	} `json:"type"`
	UpdatedAt   string    `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request"` // present only on pull requests
	// SubIssuesSummary counts sub-issues; only parents are asked for their children.
	SubIssuesSummary *struct {
		Total int `json:"total"`
	} `json:"sub_issues_summary"`
	RepositoryURL string `json:"repository_url"`
}

func (i *GitHubIssue) idString() string { return strconv.FormatInt(i.ID, 10) }

func (i *GitHubIssue) closed() bool { return i.State == stateClosed }

// sameContent compares what drives sync decisions; activity timestamps are ignored.
func (i *GitHubIssue) sameContent(o *GitHubIssue) bool {
	if i == nil || o == nil {
		return i == o
	}

	return i.ID == o.ID && i.Number == o.Number && i.Title == o.Title && i.Body == o.Body &&
		i.HTMLURL == o.HTMLURL && i.State == o.State && i.typeName() == o.typeName()
}

type gitHubUser struct {
	Login string `json:"login"`
}

type apiGitHubComment struct {
	ID        int64       `json:"id"`
	Body      string      `json:"body"`
	CreatedAt string      `json:"created_at"`
	HTMLURL   string      `json:"html_url"`
	User      *gitHubUser `json:"user"` // null for deleted accounts
}

func (g apiGitHubComment) comment() Comment {
	c := Comment{Side: SideGitHub, ID: strconv.FormatInt(g.ID, 10), Body: g.Body, Created: g.CreatedAt, HTMLURL: g.HTMLURL}
	if g.User != nil {
		c.Author = g.User.Login
	}

	return c
}

// JiraIssue is the part of a Jira issue the sync reads.
type JiraIssue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
		// Summary, Description (raw ADF), and IssueType are read for Jira issues created
		// in GitHub by jira_selection_label.
		Summary     string `json:"summary"`
		Description any    `json:"description"`
		IssueType   struct {
			ID string `json:"id"`
			// HierarchyLevel is 1 for an Epic, 0 for a standard issue, -1 for a sub-task.
			HierarchyLevel int `json:"hierarchyLevel"`
		} `json:"issuetype"`
		Updated string     `json:"updated"`
		Status  JiraStatus `json:"status"`
		Parent  *struct {
			Key string `json:"key"`
		} `json:"parent"`
	} `json:"fields"`
	Properties struct {
		Identity *IssueIdentity `json:"gh-jira-sync"`
	} `json:"properties"`
}

// done reports a status in Jira's Done category, which counts as closed on GitHub.
func (j *JiraIssue) done() bool { return j.Fields.Status.done() }

// JiraStatus is a workflow status; its category says whether work is finished.
type JiraStatus struct {
	Name           string `json:"name"`
	StatusCategory struct {
		Key string `json:"key"` // "new", "indeterminate", or "done"
	} `json:"statusCategory"`
}

func (s JiraStatus) done() bool { return s.StatusCategory.Key == "done" }

// JiraTransition is a workflow move available from an issue's current status.
type JiraTransition struct {
	ID   string     `json:"id"`
	Name string     `json:"name"`
	To   JiraStatus `json:"to"`
}

// IssueIdentity is the issue property written atomically with each created Jira issue.
type IssueIdentity struct {
	Operation string `json:"operation"`
	GitHubID  string `json:"github_id"`
}

type jiraUser struct {
	DisplayName string `json:"displayName"`
}

type jiraVisibility struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Identifier string `json:"identifier"`
}

type jiraProperty struct {
	Key string `json:"key"`
	// Each property key has its own value shape; only sd.public.comment is read.
	Value json.RawMessage `json:"value"`
}

type apiJiraComment struct {
	ID         string          `json:"id"`
	Body       any             `json:"body"` // ADF document, kept raw
	Created    string          `json:"created"`
	Author     *jiraUser       `json:"author"`
	Visibility *jiraVisibility `json:"visibility"`
	JsdPublic  *bool           `json:"jsdPublic"`
	Properties []jiraProperty  `json:"properties"`
}

// restricted reports comments limited to a role, group, or internal audience.
func (j apiJiraComment) restricted() bool {
	if (j.Visibility != nil && *j.Visibility != jiraVisibility{}) || (j.JsdPublic != nil && !*j.JsdPublic) {
		return true
	}
	for _, p := range j.Properties {
		var internal struct {
			Internal bool `json:"internal"`
		}
		if p.Key == "sd.public.comment" && json.Unmarshal(p.Value, &internal) == nil && internal.Internal {
			return true
		}
	}

	return false
}

func (j apiJiraComment) comment() Comment {
	c := Comment{Side: SideJira, ID: j.ID, Body: j.Body, Created: j.Created, Restricted: j.restricted()}
	if j.Author != nil {
		c.Author = j.Author.DisplayName
	}
	for _, p := range j.Properties {
		var identity struct {
			Operation string `json:"operation"`
		}
		if p.Key == identityKey && json.Unmarshal(p.Value, &identity) == nil {
			c.Identity = identity.Operation
		}
	}

	return c
}

// Comment is either service's comment in the one shape the engine works with.
type Comment struct {
	Side Side   `json:"side"`
	ID   string `json:"id"`
	// Body stays exactly as received: a Markdown string on GitHub, an ADF document on
	// Jira. Copy fingerprints hash it, and must match hashes from earlier releases.
	Body       any    `json:"body"`
	Created    string `json:"created"`
	Author     string `json:"author"`
	HTMLURL    string `json:"html_url,omitempty"` // GitHub only
	Restricted bool   `json:"restricted"`         // Jira only
	// Identity is the copy operation stored in a Jira comment's gh-jira-sync property;
	// it marks a copy without visible text. GitHub copies use a hidden HTML comment.
	Identity string `json:"identity,omitempty"`
}

// Text is the comment's plain text: normalized Markdown, or rendered ADF.
func (c Comment) Text() (string, error) {
	if c.Side == SideGitHub {
		body, _ := c.Body.(string)

		return normalized(body), nil
	}

	return adfText(c.Body)
}

// sameAs compares every field that drives decisions, including the raw body.
func (c Comment) sameAs(o Comment) bool { return sameJSON(c, o) }

// ProjectSnapshot is the configured project's status field and every issue item's
// value, read once per run.
type ProjectSnapshot struct {
	FieldID int64
	Options map[string]string     // option name → option ID
	Items   map[int64]ProjectItem // keyed by GitHub issue ID
	Backlog map[string]bool       // Jira issue IDs in the board's backlog, with jira_board
}

// ProjectItem is one issue's entry in the project.
type ProjectItem struct {
	ID       int64
	OptionID string // "" when the status field is unset
	Archived bool
}

// optionName returns the option's name, or "" for an unset or unknown option.
func (p *ProjectSnapshot) optionName(id string) string {
	for name, optionID := range p.Options {
		if optionID == id && id != "" {
			return name
		}
	}

	return ""
}

type apiProjectField struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	DataType string `json:"data_type"`
	Options  []struct {
		ID   string `json:"id"`
		Name struct {
			Raw string `json:"raw"`
		} `json:"name"`
	} `json:"options"`
}

type apiProjectItem struct {
	ID          int64   `json:"id"`
	ContentType string  `json:"content_type"`
	ArchivedAt  *string `json:"archived_at"`
	Content     *struct {
		ID int64 `json:"id"`
	} `json:"content"`
	Fields []struct {
		ID int64 `json:"id"`
		// A single-select value is an option object; only its ID is needed, so the
		// value is decoded on demand and also accepted as a bare option ID.
		Value json.RawMessage `json:"value"`
	} `json:"fields"`
}

func (i apiProjectItem) item(fieldID int64) ProjectItem {
	item := ProjectItem{ID: i.ID, Archived: i.ArchivedAt != nil}
	for _, f := range i.Fields {
		if f.ID != fieldID {
			continue
		}
		var option struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(f.Value, &option) == nil {
			item.OptionID = option.ID
		} else {
			_ = json.Unmarshal(f.Value, &item.OptionID) // any other shape leaves the option unset
		}
	}

	return item
}

func (i *GitHubIssue) typeName() string {
	if i.Type == nil {
		return ""
	}

	return i.Type.Name
}
