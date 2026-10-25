package ghjira

// Named types for closed sets of values, and the GitHub/Jira API shapes the sync reads.

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Side names one of the two services.
type Side string

const (
	SideGitHub Side = "github"
	SideJira   Side = "jira"
)

var sides = []Side{SideGitHub, SideJira}

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
)

// writes reports kinds that perform a remote write and count against max_operations.
func (k Kind) writes() bool {
	return k == KindCreateIssue || k == KindCreateComment || k == KindEditComment
}

// needsReview reports kinds that make the run exit 2 until resolved or acknowledged.
func (k Kind) needsReview() bool { return k == KindBlocked || k == KindUnavailable }

// passive reports kinds that apply skips entirely.
func (k Kind) passive() bool {
	switch k {
	case KindBlocked, KindNoop, KindExcluded, KindUnavailable, KindDeferred, KindAcknowledged, KindUnchanged:
		return true
	}
	return false
}

// Status is where a journaled operation stands.
type Status string

const (
	StatusPending  Status = "pending"  // intent saved; outcome unknown until confirmed
	StatusDone     Status = "done"     // destination confirmed
	StatusRejected Status = "rejected" // refused with a 4xx; nothing created, retried next run
)

func (s Status) valid() bool { return s == StatusPending || s == StatusDone || s == StatusRejected }

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

const (
	OpIssue         OpKind = "issue"
	OpGitHubComment OpKind = "comment_github"
	OpJiraComment   OpKind = "comment_jira"
)

// commentOp is the operation kind for copying a comment that originates on side.
func commentOp(side Side) OpKind {
	if side == SideGitHub {
		return OpGitHubComment
	}
	return OpJiraComment
}

func (k *OpKind) UnmarshalText(b []byte) error {
	switch v := OpKind(b); v {
	case OpIssue, OpGitHubComment, OpJiraComment:
		*k = v
		return nil
	}
	return fmt.Errorf("invalid operation kind %q", b)
}

// Outcome records what apply did that the plan could not predict.
type Outcome string

const OutcomeRejected Outcome = "rejected"

// GitHubIssue is the part of a GitHub issue the sync reads.
type GitHubIssue struct {
	ID          int64     `json:"id"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"` // null decodes as ""
	HTMLURL     string    `json:"html_url"`
	UpdatedAt   string    `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request"` // present only on pull requests
}

func (i *GitHubIssue) idString() string { return strconv.FormatInt(i.ID, 10) }

// sameContent compares what drives sync decisions; activity timestamps are ignored.
func (i *GitHubIssue) sameContent(o *GitHubIssue) bool {
	if i == nil || o == nil {
		return i == o
	}
	return i.ID == o.ID && i.Number == o.Number && i.Title == o.Title && i.Body == o.Body && i.HTMLURL == o.HTMLURL
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
		Updated string `json:"updated"`
	} `json:"fields"`
	Properties struct {
		Identity *IssueIdentity `json:"gh-jira-sync"`
	} `json:"properties"`
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
