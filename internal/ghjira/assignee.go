package ghjira

// Assignee sync (assignee_map) mirrors who an issue is assigned to. assignee_map
// names the people it covers, each a GitHub login and a Jira account ID; anyone else
// is left alone on both sides. A Jira issue has one assignee and a GitHub issue can
// have several, so GitHub holds a Jira assignee when it is among its mapped
// assignees, and a GitHub change carries its first mapped assignee to Jira. Like
// parent sync, each pair records the assignee both sides last agreed on (a Jira
// account ID, "" for nobody); the side that differs from it changed.

import (
	"slices"
	"strconv"
)

// assigneeName names an assignee in reports: "@login", or "nobody".
func (c *Config) assigneeName(account string) string {
	if account == "" {
		return "nobody"
	}
	if login, ok := c.githubLogin(account); ok {
		return "@" + login
	}

	return "an unmapped Jira account"
}

// holdsAssignee reports whether GitHub's mapped assignees hold a Jira assignee:
// include it, or, for nobody, are empty.
func holdsAssignee(mapped []string, account string) bool {
	if account == "" {
		return len(mapped) == 0
	}

	return slices.Contains(mapped, account)
}

func setAgreedAssignee(s *State, number int, account string) {
	n := strconv.Itoa(number)
	pair := s.Pairs[n]
	pair.Assignee = &account
	s.Pairs[n] = pair
}

// planAssignee plans a linked pair's assignee. A Jira assignee missing from
// assignee_map skips the pair, since GitHub cannot show it; unmapped GitHub
// assignees are ignored and kept. Changes on both sides since the agreed assignee
// block for review.
func planAssignee(c *Config, s *State, api API, pair Pair, source *GitHubIssue, jira *JiraIssue) (*Entry, error) {
	e := &Entry{GitHub: pair.GitHub, Operation: token(c, OpAssignee, pair.GitHubID), pair: pair, issue: source, jira: jira}
	mapped, _ := c.assignedAccounts(source)
	jr := jira.assignee()
	if _, known := c.githubLogin(jr); jr != "" && !known {
		e.Kind, e.Reason = KindExcluded, "Jira assignee is not in assignee_map; assignee sync skipped"

		return e, nil
	}
	gh := "" // the assignee a GitHub change carries to Jira
	if len(mapped) > 0 {
		gh = mapped[0]
	}
	e.ghAccounts, e.jiraAccount = mapped, jr
	agreed := pair.Assignee
	switch {
	case holdsAssignee(mapped, jr):
		if agreed != nil && *agreed == jr {
			return nil, nil
		}
		e.Kind, e.assignee = KindAssigneeAgreed, jr
	case agreed == nil:
		if err := planFirstAssignee(c, s, api, e, gh, jr); err != nil {
			return nil, err
		}
		if e.Kind == KindBlocked {
			return e, nil
		}
	case holdsAssignee(mapped, *agreed): // Jira moved
		e.Kind, e.Destination, e.assignee = KindSyncAssignee, SideGitHub, jr
	case jr == *agreed: // GitHub moved
		e.Kind, e.Destination, e.assignee = KindSyncAssignee, SideJira, gh
	default:
		e.block("Both sides changed since the agreed assignee " + c.assigneeName(*agreed) + ": GitHub has " +
			c.assigneeName(gh) + ", Jira has " + c.assigneeName(jr) + "; align them manually")

		return e, nil
	}
	e.Assignee = c.assigneeName(e.assignee)

	return e, nil
}

// planFirstAssignee settles a pair whose sides disagree with nothing agreed, as when
// assignee sync is turned on for pairs that already exist. An unassigned side is
// filled from the assigned one, except that a Jira issue created from GitHub keeps
// GitHub's "nobody" while its assignee is still the one Jira gave it at creation:
// that is a project default, not a choice. Two different people block for review.
func planFirstAssignee(c *Config, s *State, api API, e *Entry, gh, jr string) error {
	conflict := "GitHub assignee " + c.assigneeName(gh) + " and Jira assignee " + c.assigneeName(jr) +
		" differ, with no agreed assignee recorded"
	switch {
	case jr == "":
		e.Kind, e.Destination, e.assignee = KindSyncAssignee, SideJira, gh
		e.Reason = conflict + "; Jira is unassigned, so it follows"
	case gh != "":
		e.block(conflict + "; align them manually")
	default:
		fromGitHub := originSide(c, s, e.pair) == SideGitHub
		changed := ""
		if fromGitHub {
			var err error
			if changed, err = api.JiraFieldChanged(e.pair.JiraID, jiraAssigneeField); err != nil {
				return err
			}
		}
		if fromGitHub && changed == "" {
			e.Kind, e.Destination, e.assignee = KindSyncAssignee, SideJira, ""
			e.Reason = conflict + "; Jira's assignee has not changed since the issue was created from GitHub, so Jira follows"
		} else {
			e.Kind, e.Destination, e.assignee = KindSyncAssignee, SideGitHub, jr
			e.Reason = conflict + "; GitHub is unassigned, so it follows"
		}
	}

	return nil
}

// applyAssignee sets one side's assignee to match the other, re-checking both sides
// before and after. Like status changes it is idempotent and not journaled.
func applyAssignee(c *Config, s *State, api API, e *Entry, save func(*State) error) error {
	current := func() (*GitHubIssue, []string, string, error) {
		issue, err := api.Issue(e.GitHub)
		if err != nil {
			return nil, nil, "", err
		}
		if issue == nil || issue.idString() != e.pair.GitHubID {
			return nil, nil, "", rerunErr("Linked GitHub issue vanished after plan; retried next run")
		}
		jira, err := api.JiraIssue(e.pair.JiraID)
		if err != nil {
			return nil, nil, "", err
		}
		if jira == nil {
			return nil, nil, "", rerunErr("Linked Jira issue vanished after plan; retried next run")
		}
		mapped, _ := c.assignedAccounts(issue)

		return issue, mapped, jira.assignee(), nil
	}
	issue, gh, jr, err := current()
	if err != nil {
		return err
	}
	if holdsAssignee(gh, e.assignee) && jr == e.assignee {
		return alreadyDone(s, e, save, func() { setAgreedAssignee(s, e.GitHub, e.assignee) })
	}
	if !slices.Equal(gh, e.ghAccounts) || jr != e.jiraAccount {
		return rerunErr("Assignee changed after plan; retried next run")
	}
	if e.Destination == SideGitHub {
		// Mapped assignees are replaced by the target; anyone unmapped stays.
		_, logins := c.assignedAccounts(issue)
		if login, ok := c.githubLogin(e.assignee); ok {
			logins = append(logins, login)
		}
		err = api.SetGitHubAssignees(e.GitHub, logins)
	} else {
		err = api.SetJiraAssignee(e.pair.JiraID, e.assignee)
	}
	if status, refused := refusedStatus(err); refused && itemRejections[status] {
		hint := "; check that " + e.Assignee + " can be assigned issues there"
		if e.assignee == "" {
			hint = "; check that the issue can be left unassigned"
		}
		e.Outcome, e.Reason = OutcomeRejected, "Assignee change rejected with HTTP "+strconv.Itoa(status)+hint+". Retried next run"

		return nil
	}
	if err != nil {
		return err
	}
	if _, gh, jr, err = current(); err != nil {
		return err
	}
	if !holdsAssignee(gh, e.assignee) || jr != e.assignee {
		if e.Destination == SideGitHub {
			// GitHub drops a login that cannot be assigned instead of refusing it.
			e.Outcome, e.Reason = OutcomeRejected, "GitHub did not assign "+e.Assignee+
				"; check that they can be assigned issues in the repository. Retried next run"

			return nil
		}

		return rerunErr("Assignee change not confirmed; GitHub has %s, Jira has %s; retried next run",
			c.assigneeName(firstOr(gh)), c.assigneeName(jr))
	}
	setAgreedAssignee(s, e.GitHub, e.assignee)

	return save(s)
}

// firstOr returns the first account, or "" for none.
func firstOr(accounts []string) string {
	if len(accounts) == 0 {
		return ""
	}

	return accounts[0]
}
