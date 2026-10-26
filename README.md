# <img src="docs/icon.svg" alt="" width="32" height="32" align="top"> GitHub ↔ Jira issue sync

Creates Jira Cloud issues from GitHub issues labeled **`sync-jira`**, then copies
original comments and their edits in both directions. Optionally it also mirrors
open/closed status, a GitHub Project's Status field, parent links (Epics and
sub-issues), and creates GitHub issues from labeled Jira issues. It is a single Go
binary with a reusable GitHub Actions workflow.

* Title and description are copied once, at creation.
* Nothing is ever deleted, and attachments are not copied.
* Edits to copies are reported for review; originals are never modified.
* Dry-run by default; `--apply` is required for remote writes.

See [the design](docs/design.md) for identity, recovery, and tradeoffs.

## Quick start

Copy [`sync.example.json`](sync.example.json) to `sync.json` and set the repository,
Jira site, project key, and issue type ID. Then set credentials:

| Environment variable | Purpose |
| --- | --- |
| `GH_TOKEN` | GitHub token with Issues read/write on the repository |
| `GH_PROJECT_TOKEN` | Projects read/write; only with `project_status` |
| `JIRA_EMAIL`, `JIRA_API_TOKEN` | Jira Cloud service account |

The Jira account needs Browse Projects, Create Issues, Add Comments, Edit Own
Comments, and access to issue properties. Add Transition Issues for `status_sync` or
`project_status`, Edit Issues for `sync_parents`, and board browse and rank
permissions for backlog mode. Jira Service Management projects are not supported.

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
  schedule:
    - cron: '17 * * * *'
permissions:
  contents: write
jobs:
  sync:
    uses: cccteam/gh-jira-sync/.github/workflows/sync-reusable.yml@v0.1.2 # x-release-please-version
    with:
      app-client-id: Iv23liExampleClientId
      apply: ${{ inputs.apply || (github.event_name == 'schedule' && vars.SYNC_APPLY == 'true') }}
      initialize: ${{ inputs.initialize || false }}
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
| `version` | the pinned release | Release to run; leave unset |
| `tool-ref` | empty | Build from this ref instead, to test unreleased commits |
| `app-client-id` | empty | GitHub App that supplies the GitHub tokens |

| Secret | Purpose |
| --- | --- |
| `jira-email`, `jira-api-token` | Required. Jira Cloud service account |
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
    uses: cccteam/gh-jira-sync/.github/workflows/sync-reusable.yml@v0.1.2 # x-release-please-version
    with:
      config: sync/${{ matrix.deployment }}.json
      state-branch: sync-state-${{ matrix.deployment }}
```

Each state branch is bound to its config's repository and Jira project, and must
contain only `state.json`. Never point two deployments at the same repository and
project pair: they would keep separate journals and could duplicate issues.

## Configuration

Unknown keys fail validation.

| Option | Default / meaning |
| --- | --- |
| `repository` | Required `owner/name` |
| `jira_url` | Required Jira Cloud HTTPS site URL |
| `jira_project` | Required project key, e.g. `ENG` |
| `jira_issue_type` | Required issue type **ID**, e.g. `"10001"` |
| `jira_issue_types` | `{}`; GitHub issue type name → Jira issue type ID, e.g. `{"Bug": "10004"}` |
| `selection_label` | `sync-jira`; `null` selects all issues |
| `issue_state` | `open`; or `closed` or `all` |
| `comments` | `true`; copy new comments and edits to originals |
| `comments_since` | `null` backfills; an ISO timestamp limits which existing comments are copied |
| `status_sync` | `null`; `{"close_transition": "Done", "reopen_transition": "Reopen"}` mirrors open/closed |
| `project_status` | `null`; mirrors a GitHub Project field instead of `status_sync` |
| `sync_parents` | `false`; mirror GitHub sub-issue parents and Jira parents |
| `jira_subtask_type` | `null`; sub-task type ID for issues nested under a standard Jira issue |
| `jira_selection_label` | `null`; Jira issues with this label are created in GitHub |
| `jira_create_fields` | Extra Jira create fields, such as required custom fields or `labels` |
| `max_operations` | `20` writes per run; the rest are `deferred` to the next run |
| `max_pages` | `100` pages per listing; an incomplete scan fails the run |
| `full_rescan_hours` | `24`; longest an unchanged pair goes without a full comment scan |
| `github_url` | `https://api.github.com`; other API bases work for CLI runs only |

The label and state filters choose which issues are **created**. Once a pair is
linked, it keeps syncing even if the issue closes or loses its label.

List a project's issue type IDs with:

```sh
curl -s -u "$JIRA_EMAIL:$JIRA_API_TOKEN" "$JIRA_SITE/rest/api/3/project/<KEY>" | jq '.issueTypes[] | {id, name, subtask}'
```

## Features

**Issue types.** `jira_issue_types` picks the Jira type from the GitHub issue type
(names ignore case); anything unmapped uses `jira_issue_type`. The type is set at
creation only.

**Issue status.** With `status_sync`, closing or reopening on GitHub applies the named
Jira transition, and moving a Jira issue into or out of the Done category closes or
reopens the GitHub issue. Whichever side changed wins. A pair whose sides disagree
with no recorded agreement (for example, pairs that existed before status sync was
turned on) is blocked until you align it by hand.

**Project status.** `project_status` keeps a GitHub Project's single-select field and
the Jira status in step, in both directions:

```json
"project_status": {
  "owner": "cccteam",
  "project_number": 7,
  "map": [
    {"project": "Todo",        "jira": ["To Do", "Backlog"]},
    {"project": "In Progress", "jira": "In Progress"},
    {"project": "Done",        "jira": ["Done", "Won't Do"]}
  ]
}
```

Other keys: `owner_type` (`org` or `user`), `field` (default `Status`),
`add_to_project` (default `true`), `map[].transition` to pick among several
transitions, and `jira_board` with one `map[].jira_backlog` option to mirror a board's
backlog instead of a status. A status or option missing from `map` blocks that issue.
Turn on the project's **Item closed** and **Item reopened** workflows so GitHub
close/reopen reaches Jira through the project.

**Epics and parents.** With `sync_parents`, GitHub sub-issue parents and Jira parents
are mirrored between linked issues. Add an Epic issue type in GitHub and map it in
`jira_issue_types` so Epics create Epics. Jira's hierarchy refusals are reported as
`rejected`. Set `jira_subtask_type` so an issue nested under a Task is created as a
Jira sub-task; it waits until its parent is in Jira.

**Jira → GitHub creation.** With `jira_selection_label`, each labeled Jira issue gets
a GitHub issue with its summary and description. The pair then syncs like any other.
Descriptions with media or tables block that issue.

**Comments.** Copies start with a header naming the original author and linking to
the original, and carry a hidden identity marker. Don't remove either when editing a
copy, or the comment is blocked for review. GitHub Markdown is copied literally to
Jira; Jira comments become plain text on GitHub. Restricted Jira comments are skipped.

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
`sync_project_status`, and `sync_parent`; an `outcome` of `rejected` means the service
refused it and it is retried next run. The kinds that need a person are:

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

That run also performs any other planned writes. For an Actions deployment, run it
from a clone of the calling repository with `--git-state-branch <branch>` and the same
credentials.

Exit codes: `0` success (including deferred writes), `2` something needs review,
`1` configuration, API, or persistence error.

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
