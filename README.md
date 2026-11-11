# <img src="docs/icon.svg" alt="" width="32" height="32" align="top"> GitHub ↔ Jira issue sync

Creates Jira Cloud issues from GitHub issues labeled **`sync-jira`**, then copies
original comments and their edits in both directions. Optionally it also mirrors
open/closed status, a GitHub Project's Status field, parent links (Epics and
sub-issues), and later title and description edits, and creates GitHub issues from
labeled Jira issues. It is a single Go binary with a reusable GitHub Actions workflow.

* Title and description are copied at creation, and later edits only with `sync_content`
  (titles both ways, descriptions from the issue a pair started on).
* Nothing is ever deleted, and attachments are not copied.
* Edits to copies are reported for review; originals are never modified.
* Dry-run by default; `--apply` is required for remote writes.

See [the design](docs/design.md) for identity, recovery, and tradeoffs.

## Quick start

Copy [`sync.example.json`](sync.example.json) to `sync.json` and set the repository,
Jira site, project key, and issue type. Then set credentials:

| Environment variable | Purpose |
| --- | --- |
| `GH_TOKEN` | GitHub token with Issues read/write on the repository |
| `GH_PROJECT_TOKEN` | Projects read/write; only with `project_status` |
| `JIRA_EMAIL`, `JIRA_API_TOKEN` | Jira account email and API token; see [Jira account](#jira-account) |
| `JIRA_CLIENT_ID`, `JIRA_CLIENT_SECRET` | Instead: a service account's OAuth 2.0 credential |

The Jira account needs Browse Projects, Create Issues, Add Comments, Edit Own
Comments, and access to issue properties. Add Transition Issues for `status_sync` or
`project_status`, and Edit Issues for `sync_parents` or `sync_content`. Jira Service
Management projects are not supported.

### Jira account

Use an [Atlassian service account](https://support.atlassian.com/user-management/docs/understand-service-accounts/)
where you can. It does not use a Jira seat, but it needs Atlassian Guard Standard or
Enterprise.

1. Create the service account in Atlassian Administration, give it Jira access, and
   grant it the project permissions above.
2. Create one credential for it, with the scopes `read:jira-work` and
   `write:jira-work`. Scopes cannot be changed later. Either:
   * an [OAuth 2.0 credential](https://support.atlassian.com/user-management/docs/create-oauth-2-0-credential-for-service-accounts/):
     set `JIRA_CLIENT_ID` and `JIRA_CLIENT_SECRET`. Each run trades them for an access
     token, which lasts an hour, and renews it before it runs out; or
   * an [API token](https://support.atlassian.com/user-management/docs/manage-api-tokens-for-service-accounts/):
     set `JIRA_EMAIL` to the service account's email and `JIRA_API_TOKEN` to the
     token. It expires, after one year by default.
3. Set `jira_cloud_id` to the [site's cloud ID](https://support.atlassian.com/jira/kb/retrieve-my-atlassian-sites-cloud-id/),
   for example from `https://<site>.atlassian.net/_edge/tenant_info`. Service account
   credentials work only through Atlassian's API gateway, so this sends API calls
   there. Keep `jira_url` as the site; links in copies still point to it.

Set one pair of Jira variables, never both; a run with both, or with only half of
one, stops before contacting Jira. If an access token cannot be obtained, the run
stops and the write it was for is `rejected` and retried next run.

Each run first confirms that the cloud ID belongs to `jira_url`. An existing deployment
can switch to a service account without new state, since state is bound to `jira_url`.
Jira copies the old account wrote can be edited only by that account unless the new
one also has Edit All Comments. Without it, later edits to those originals are
`rejected` and retried each run.

A dedicated ordinary Atlassian user with an
[API token](https://support.atlassian.com/atlassian-account/docs/manage-api-tokens-for-your-atlassian-account/)
also works. Leave `jira_cloud_id` unset for it.

```sh
go install github.com/cccteam/gh-jira-sync@latest
gh-jira-sync --config sync.json --init-state            # dry run: see the plan
gh-jira-sync --config sync.json --init-state --apply    # first write run
gh-jira-sync --config sync.json --apply                 # later runs
```

Run again after creating issues to copy their comments. Linux and macOS only.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--config` | `sync.json` | Configuration file |
| `--apply` | off | Perform remote writes |
| `--init-state` | off | Allow creating a new journal (first run only) |
| `--git-state-branch` | none | Keep state on this branch of `origin` (`sync-state` or `sync-state-<name>`); required on ephemeral runners, and then the branch is authoritative |
| `--state` | `.sync-state/state.json` | Local state file |
| `--report` | `sync-report.json` | Report file |
| `--acknowledge OPERATION` | none | Silence a reviewed `blocked` or `unavailable` entry (repeatable; saved only with `--apply`) |
| `--summarize FILE` | none | Print a Markdown summary of a report file, linking issues from `--config`, and exit |
| `--version` | | Print the release and exit |

## GitHub Actions

Call the reusable workflow from the repository being synced. Pin a release; that
release's binary is downloaded and checked against its checksum and build
provenance attestation before it runs.

```yaml
name: Sync GitHub and Jira
on:
  workflow_dispatch:
    inputs:
      apply: { type: boolean, default: false }
      initialize: { type: boolean, default: false }
      acknowledge: { type: string, default: '' }
  schedule:
    - cron: '17 * * * *'
permissions:
  contents: write
jobs:
  sync:
    uses: cccteam/gh-jira-sync/.github/workflows/sync-reusable.yml@v0.2.7 # x-release-please-version
    with:
      app-client-id: Iv23liExampleClientId
      apply: ${{ inputs.apply || (github.event_name == 'schedule' && vars.SYNC_APPLY == 'true') }}
      initialize: ${{ inputs.initialize || false }}
      acknowledge: ${{ inputs.acknowledge || '' }}
    secrets:
      jira-email: ${{ secrets.JIRA_EMAIL }}
      jira-api-token: ${{ secrets.JIRA_API_TOKEN }}
      app-private-key: ${{ secrets.GH_JIRA_SYNC_APP_KEY }}
```

First dispatch with **initialize** only and read the job summary, then with
**initialize** and **apply**. Scheduled runs stay dry-run until the repository
variable `SYNC_APPLY=true` is set. Dependabot's `github-actions` updates keep the
pin current. Releases v0.1.1 and later carry binaries; v0.1.0 does not.

| Input | Default | Meaning |
| --- | --- | --- |
| `config` | `sync.json` | Config path in the calling repository |
| `state-branch` | `sync-state` | State branch: `sync-state` or `sync-state-<name>` |
| `apply` | `false` | Perform writes |
| `initialize` | `false` | Allow creating new state (first run only) |
| `acknowledge` | empty | Report operation IDs to [acknowledge](#the-report), separated by spaces or commas; saved only with `apply` |
| `fail-on-review` | `true` | Fail the run when entries need review (exit code 2); `false` passes it with a warning |
| `version` | the pinned release | Release to run; leave unset |
| `tool-ref` | empty | Build from this ref instead, to test unreleased commits |
| `app-client-id` | empty | GitHub App that supplies the GitHub tokens |

| Secret | Purpose |
| --- | --- |
| `jira-email`, `jira-api-token` | Jira account email and API token |
| `jira-client-id`, `jira-client-secret` | Instead: a service account's OAuth 2.0 credential |
| `app-private-key` | The GitHub App's private key, with `app-client-id` |
| `github-token` | Instead of the App: Issues read/write on the configured repository |
| `project-token` | Instead of the App: Projects read/write, for example for a user-owned project |

The caller needs `contents: write` to push the state branch, plus `issues: write`
when it uses neither the App nor `github-token`. The job runs only from the default
branch and never cancels an active run.

### GitHub App (recommended)

One GitHub App supplies every GitHub token, so no personal access tokens are needed.
Each run mints short-lived tokens limited to what they are for: Issues read/write on
the configured repository, and, with `project_status`, organization Projects
read/write plus Issues read for the project's owner. Copied comments show the App as
their author.

1. Create a GitHub App owned by the organization, with the webhook off, **Issues:
   Read and write**, and **Projects: Read and write** (organization) if you use
   `project_status`.
2. Install it on only the repositories being synced.
3. Store its private key as an organization or environment secret limited to those
   repositories, and pass its Client ID as `app-client-id`.

### Several deployments

A deployment is one config file plus one state branch, syncing one repository with
one Jira project. A hub repository can run several with a matrix:

```yaml
    strategy:
      fail-fast: false
      matrix:
        deployment: [web, api]
    uses: cccteam/gh-jira-sync/.github/workflows/sync-reusable.yml@v0.2.7 # x-release-please-version
    with:
      config: sync/${{ matrix.deployment }}.json
      state-branch: sync-state-${{ matrix.deployment }}
```

Each state branch is bound to its config's repository and Jira project, and must
contain only `state.json`. Never point two deployments at the same repository and
project pair: they would keep separate journals and could duplicate issues.

## Configuration

`sync.json` is one JSON object. Only the first four options are required; leave out
any other option to get its default. Unknown keys and wrong types fail validation.
[`sync.example.json`](sync.example.json) lists every option at its default.

A **pair** is a GitHub issue and the Jira issue linked to it. The issue a pair
**started on** is the original; the other is its **copy**.

### Where to sync

| Option | Default | Meaning |
| --- | --- | --- |
| `repository` | required | The GitHub repository, as `owner/name`. |
| `jira_url` | required | Your Jira Cloud site, such as `https://your-company.atlassian.net`. Links in copies point here. |
| `jira_project` | required | Key of the Jira project issues are created in, such as `ENG`. |
| `jira_default_issue_type` | required | Jira issue type for issues created from GitHub, by name (`"Task"`) or ID (`"10001"`), unless `jira_issue_types` picks another. Earlier releases called it `jira_issue_type`, which still works. |
| `jira_cloud_id` | `null` | The site's cloud ID. Required with a [service account](#jira-account)'s OAuth credential, which can only call Jira through Atlassian's API gateway; leave `null` with an email and API token. |
| `github_url` | `https://api.github.com` | GitHub API base URL. Change it only for GitHub Enterprise Server, from the CLI; the Actions workflow supports github.com only. |

Changing `repository`, `jira_url`, `jira_project`, or `github_url` makes existing
state invalid, since every pair and copy is tied to them. Start a new state branch
for a new destination.

### Which issues are created

These options decide which issues get a copy. Once a pair is linked it keeps
syncing, even if the issue later closes or loses its label.

| Option | Default | Meaning |
| --- | --- | --- |
| `selection_label` | `"sync-jira"` | Only GitHub issues with this label are created in Jira. **`null` selects every issue in the repository.** GitHub issues created from Jira get this label too. |
| `issue_state` | `"open"` | Which issues are created: `"open"`, `"closed"`, or `"all"`. On the Jira side (`jira_selection_label`), open means not in the Done category. |
| `jira_issue_types` | `{}` | Picks the Jira type from the GitHub issue type, such as `{"Bug": "Bug", "Feature": "Story"}`. GitHub names ignore case; unmapped and untyped issues use `jira_default_issue_type`. Also used in reverse to set the GitHub type of issues created from Jira. |
| `jira_create_fields` | `{}` | Extra fields set on every Jira issue created, such as required custom fields or `{"labels": ["from-github"]}`. They may not set `project`, `issuetype`, `summary`, `description`, or `parent`. |
| `jira_selection_label` | `null` | Creates a GitHub issue for each Jira issue in the project with this label. **`null` turns Jira → GitHub creation off** (unlike `selection_label`, where `null` means all issues). |

### What is kept in sync

| Option | Default | Meaning |
| --- | --- | --- |
| `comments` | `true` | Copy each new comment to the other side, and copy later edits of the original comment to its copy. `false` copies no comments. |
| `comments_since` | `null` | Copy only comments created at or after this ISO timestamp with timezone, such as `"2026-01-01T00:00:00Z"`. `null` also copies comments that existed before the pair was linked. |
| `sync_content` | `false` | Copy later title and description edits. Titles sync both ways; descriptions only from the original to the copy. See [Titles and descriptions](#features). |
| `sync_parents` | `false` | Mirror parent links: GitHub sub-issues and Jira parents, such as an Epic and its stories. |
| `jira_subtask_type` | `null` | Jira sub-task type, by name or ID. When set, a GitHub sub-issue of an issue that is a standard Jira issue (such as a Task) is created as a Jira sub-task. Needs `sync_parents`. |
| `status_sync` | `null` | Mirror open/closed: `{"close_transition": "Done", "reopen_transition": "Reopen"}` names the Jira transitions applied when the GitHub issue closes or reopens. Jira's Done category closes the GitHub issue. |
| `project_status` | `null` | Mirror a GitHub Project's Status field and the Jira status instead; see [Project status](#features). Use either this or `status_sync`, not both. |
| `jira_owned_issue_types` | `[]` | Jira issue types, by name or ID, that only Jira decides, such as `["Epic"]`. Their open/closed state, project status, and parent flow from Jira to GitHub only; see [Jira-owned issue types](#features). Needs `status_sync`, `project_status`, or `sync_parents`. |
| `assignee_map` | `null` | Mirror assignees for the people listed, as `{"github-login": "jira-account-id"}`. `null` turns assignee sync off. See [Assignees](#features). |
| `ignored_jira_parents` | `[]` | Jira issue keys, such as `["ITP-2019"]`, whose children skip parent sync instead of blocking for review. Use it for a parent that will never be in GitHub, such as an initiative in another project. Needs `sync_parents`. |

### Run limits

| Option | Default | Meaning |
| --- | --- | --- |
| `max_operations` | `20` | Most remote writes in one run. Later writes are reported as `deferred` and done on the next run, which limits the damage a bad configuration can do. |
| `max_pages` | `100` | Most pages (of 100 items) read from any one listing. A listing longer than this fails the run, since a partial scan could look like deletions. |
| `full_rescan_hours` | `24` | Pairs with no new activity are skipped, but each is fully rescanned at least this often, since comment edits don't always update an issue's activity time. |

Jira issue types can be given by name (ignoring case) or by ID; a value of only
digits is always an ID. Names are looked up in the project at the start of each run,
and a name that matches more than one type stops the run. List a project's issue
types with:

```sh
curl -s -u "$JIRA_EMAIL:$JIRA_API_TOKEN" "$JIRA_SITE/rest/api/3/project/<KEY>" | jq '.issueTypes[] | {id, name, subtask}'
```

## Features

**Issue types.** `jira_issue_types` picks the Jira type from the GitHub issue type
(names ignore case); anything unmapped uses `jira_default_issue_type`. The type is set
at creation only.

**Issue status.** With `status_sync`, closing or reopening on GitHub applies the named
Jira transition, and moving a Jira issue into or out of the Done category closes or
reopens the GitHub issue. Whichever side changed wins. A pair whose sides disagree
with no recorded agreement (for example, pairs that existed before status sync was
turned on) is blocked until you align it by hand. With `project_status`, such pairs,
and pairs where both sides changed between runs, follow whichever side changed later;
changes less than a minute apart still block.

**Project status.** `project_status` keeps a GitHub Project's single-select field and
the Jira status in step, in both directions:

```json
"project_status": {
  "owner": "cccteam",
  "project_number": 7,
  "map": [
    {"project": "Backlog",     "jira": "Backlog"},
    {"project": "Todo",        "jira": "To Do"},
    {"project": "In Progress", "jira": "In Progress"},
    {"project": "Done",        "jira": ["Done", "Won't Do"]}
  ]
}
```

Statuses and transitions are matched by name, exactly as Jira shows them. A Backlog
status maps like any other, as above. Other keys: `owner_type` (`org` or `user`),
`field` (default `Status`), `add_to_project` (default `true`), `map[].transition` to
pick among several transitions. A status or option missing from `map` blocks that
issue.

Board backlogs are not tracked: on a team-managed board with the backlog turned on,
add a Backlog status to the workflow and map it, as above. Releases up to v0.1.3 had
a `jira_board` and `map[].jira_backlog` mode for this, and configurations that still
use it now fail validation. To switch, first move the issues in the board's backlog
into the Backlog status, then change the configuration; the other order makes every
backlog issue look moved to the option for its old status.
Turn on the project's **Item closed** and **Item reopened** workflows so GitHub
close/reopen reaches Jira through the project.
When the sync itself closes or reopens an issue to follow Jira and a workflow then sets
another option, such as Done for Jira's Won't Do, the sync sets Jira's option back
instead of moving Jira. It recognizes the workflow by its actor in the issue's
timeline, which the project token reads.

**Epics and parents.** With `sync_parents`, GitHub sub-issue parents and Jira parents
are mirrored between linked issues. Add an Epic issue type in GitHub and map it in
`jira_issue_types` so Epics create Epics. Jira's hierarchy refusals are reported as
`rejected`. Set `jira_subtask_type` so an issue nested under a Task is created as a
Jira sub-task; it waits until its parent is in Jira.

**Assignees.** With `assignee_map`, an issue's assignee is mirrored between GitHub and
Jira for the people it lists:

```json
"assignee_map": {
  "octocat": "712020:2c5f3e6a-1b2c-4d5e-8f90-123456789abc"
}
```

Keys are GitHub logins (case is ignored); values are Jira account IDs, which appear in
the URL of the person's Jira profile. Each person is listed once. Whoever changed the
assignee since the last run wins, as for parents; changes on both sides are `blocked`.
A Jira issue has one assignee and a GitHub issue can have several: GitHub agrees with
Jira while Jira's assignee is among its listed assignees, and a GitHub change sends
its first listed assignee to Jira. A change from Jira replaces GitHub's listed
assignees and keeps anyone unlisted. A Jira assignee not in the map is `excluded`.
New Jira issues are created with the GitHub assignee, so `jira_create_fields` cannot
set `assignee` too. A Jira issue created from GitHub with no assignee is unassigned
again if the Jira project gave it a default assignee. When the map is first added,
an unassigned side is filled from the assigned one, and two different people are
`blocked`. GitHub ignores a login that cannot be assigned issues in the repository,
which is reported as `rejected`.

**Jira-owned issue types.** List issue types in `jira_owned_issue_types` when Jira is
where they are planned, for example Epics. For those issues GitHub always follows Jira:
a GitHub close or reopen, a project status change (even to an unmapped option), or a
sub-issue parent change is set back to Jira's value instead of being copied to Jira,
and conflicts follow Jira rather than the later change. Jira's own changes still reach
GitHub as usual. A GitHub issue whose type maps to an owned Jira type is `excluded`
from creation in Jira; create it in Jira and let `jira_selection_label` bring it to
GitHub. Titles and descriptions follow `sync_content` as for any issue. The type is
read from the linked Jira issue, so a story under an owned Epic is not owned.

**Ignored Jira parents.** A Jira parent that is not linked to a GitHub issue normally
blocks parent sync for its children, every run. List it in `ignored_jira_parents` when
it never will be, such as an initiative in another project: its children's parent
sync is reported as `excluded` and needs no review. Their other syncing is unchanged.

**Titles and descriptions.** A GitHub description becomes Jira rich text: headings,
lists, task lists, quotes, code, tables, links, and inline formatting carry over, and
anything else stays as literal text. With `sync_content`, later edits to the title or
description of the issue a pair started on are copied to the other issue, in the same
form as at creation. A title edited on the copy is copied back; if both titles were
edited, the later edit wins (edits under a minute apart are `blocked`). A description
edited on the copy is never copied back or overwritten, since Jira's rich text does not
convert back to Markdown without losing formatting: it is `blocked` until the same change
is made on the original or the copy edit is undone. The first run after turning it on records copies that already match and
replaces any that differ from the original; run without `--apply` first to see which.

**Jira → GitHub creation.** With `jira_selection_label`, each labeled Jira issue gets
a GitHub issue with its summary and description. The pair then syncs like any other.
Descriptions with media or tables block that issue.

**Comments.** Copies start with a header naming the original author and linking to
the original, and carry a hidden identity marker. Don't remove either when editing a
copy, or the comment is blocked for review. GitHub Markdown is copied literally to
Jira; Jira comments become plain text on GitHub, and those with media or tables are
blocked rather than copied with content missing. Restricted Jira comments are skipped.

## The report

Every run prints a report, writes it to `--report`, and in Actions adds it to the job
summary. It holds IDs and decisions, never issue or comment text.

```json
{
  "mode": "apply",
  "planned_writes": 2,
  "deferred_writes": 0,
  "exit_code": 2,
  "entries": [
    {"github": 12, "operation": "3f9c…", "kind": "create_comment",
     "source": "github", "comment_id": "2210", "destination": "jira"},
    {"github": 15, "operation": "c04d…", "kind": "unavailable",
     "reason": "Linked issue deleted, transferred, moved, or inaccessible; comment sync paused"}
  ]
}
```

Each entry has a `kind`. Writes are `create_issue`, `create_github_issue`,
`create_comment`, `edit_comment`, `sync_status`, `add_to_project`,
`sync_project_status`, `sync_parent`, `sync_assignee`, and `sync_content` (with `field`
set to `title` or `description`); an `outcome` of `rejected` means the service
refused it and it is retried next run, and an `outcome` of `retry` means one side
changed while the run was writing, so the entry was skipped and is planned again next
run. The kinds that need a person are:

| Kind | Meaning |
| --- | --- |
| `blocked` | Needs review; `reason` says why |
| `unavailable` | An issue or comment was deleted, moved, or became inaccessible; nothing is deleted or recreated |
| `acknowledged` | A reviewed `blocked` or `unavailable` entry; quiet until its reason changes |

Other kinds (`noop`, `unchanged`, `deferred`, `excluded`, and the `*_agreed`,
`link_*`, `recover_comment`, `confirm_comment`, and `reconciled` state updates) need
no action. To acknowledge an entry after reviewing it:

```sh
gh-jira-sync --config sync.json --apply --acknowledge <operation>
```

That run also performs any other planned writes. In Actions, dispatch the workflow
with **apply** and the operation IDs in **acknowledge** (pass the reusable workflow's
`acknowledge` input through, as in the example above). An ID that no longer matches a
blocked or unavailable entry fails the run without writing.

Exit codes: `0` success (including deferred writes and `retry` outcomes), `2`
something needs review, `1` configuration, API, or persistence error.

In Actions, the job summary shows the report as tables: what needs review (with the
operation IDs to acknowledge), what will be retried or was rejected, the changes made,
and what was deferred, with the full JSON report collapsed below.

## Safety and recovery

Pilot in a sandbox and review the first plans. Every creation is journaled before the
request, and a lost response is recovered by finding the destination's hidden
identity on the next run. If it cannot be found, the operation is **blocked instead
of created twice**. Writes the service refuses (4xx) are retried next run without
editing state.

For a blocked pending operation, rerun a dry run after Jira indexing catches up. Only
if you have verified nothing was created, remove **that one pending operation** from
`state.json` (on the state branch in Actions). Never initialize over lost production
state or restore an old checkpoint: that can erase pending intents and allow
duplicates.

State holds IDs, URLs, and fingerprints, but no comment text or tokens. Protect the
state branch, workflow, and config from untrusted edits.

## Development

```sh
go test -race ./...
```

Tests use fake APIs and need no credentials. Releases are cut by release-please from
Conventional Commit pull request titles; see [the design](docs/design.md#releases-and-distribution).
