# GitHub → Jira creation, two-way comments and status

Creates Jira Cloud issues from GitHub issues labeled **`sync-jira`**, then copies
original comments and their edits in both directions. A single Go binary (Go 1.27+,
standard library only) with a ready-to-use GitHub Actions workflow.

* Issue title and description are copied at creation only.
* Optionally, closing or reopening an issue on either side does the same on the
  other (see [Issue status](#issue-status)).
* Or, with a GitHub Project, its Status field and Jira workflow statuses stay in step
  (see [Project status](#project-status)).
* Original comment edits update the existing copy. Edits to copies are reported
  and preserved for review; original comments are never modified by the utility.
* No issue/comment deletions, reverse issue creation, or attachment copying.
* Dry-run by default; explicit `--apply` required for remote writes.

See [the design](docs/design.md) for identity, failure recovery, and tradeoffs.

## Quick start

Copy `sync.example.json` to `sync.json`, and set your repository, Jira Cloud site,
project key, and issue type ID. Keep the `sync-jira` label filter for a small pilot.

Set credentials through your secret manager or shell environment:

| Environment variable | Purpose |
| --- | --- |
| `GH_TOKEN` | GitHub token with issues read/write access |
| `JIRA_EMAIL` | Jira Cloud service account email |
| `JIRA_API_TOKEN` | API token for that account |

The Jira account needs Browse Projects, Create Issues, Add Comments, Edit Own
Comments, and access to issue properties, plus Transition Issues when status sync
is on. Jira Software and business projects are
supported; Jira Service Management projects are rejected to avoid mishandling
internal comments. HTTPS is mandatory and credentials never follow redirects.

```sh
# Build the binary.
go build -o gh-jira-sync .

# Local durable-disk usage: inspect without changing either service.
./gh-jira-sync --config sync.json --init-state

# First apply: creates Jira issues and the local operation journal.
./gh-jira-sync --config sync.json --init-state --apply

# Next pass copies comments for the newly established issue mappings.
./gh-jira-sync --config sync.json --apply

# Subsequent dry-run, using the existing journal.
./gh-jira-sync --config sync.json

# Offline tests, including real Git state persistence and Python-parity vectors.
go test -race ./...
```

To install from GitHub instead, while the repository is private, tell Go to skip the
public module proxy and give Git access to `cccteam` repositories:

```sh
GOPRIVATE=github.com/cccteam/* go install github.com/cccteam/gh-jira-sync@latest
```

Flags accept one or two dashes (`-apply` or `--apply`).
Supported platforms are Linux/macOS (the local lock uses `flock`). No live
credentials are needed to run tests.

State and identity formats stay compatible with the earlier Python release:
operation tokens and content fingerprints are byte-identical (checked against vectors
in `internal/ghjira/testdata/golden.json`), so existing `sync-state` branches, Jira
issues, and copies keep working. Issues and copies created now carry no visible
markers; older labels and marker lines are still recognized.

## GitHub Actions

All runs go through the reusable workflow
[`.github/workflows/sync-reusable.yml`](.github/workflows/sync-reusable.yml). A
**deployment** is one config file plus one state branch, syncing one GitHub
repository with one Jira project.

| Input | Default | Meaning |
| --- | --- | --- |
| `config` | `sync.json` | Config path in the calling repository |
| `state-branch` | `sync-state` | State branch in the calling repository: `sync-state` or `sync-state-<name>` |
| `apply` | `false` | Perform writes; otherwise dry-run |
| `initialize` | `false` | Allow creating new state (first run only) |
| `tool-ref` | `main` | Ref of this repository to build; pin it to the ref in `uses:` |
| `app-client-id` | empty | GitHub App that supplies every GitHub token; see [GitHub App](#github-app-recommended) |

| Secret | Required | Purpose |
| --- | --- | --- |
| `jira-email`, `jira-api-token` | yes | Jira Cloud service account |
| `app-private-key` | with `app-client-id` | The GitHub App's private key |
| `github-token` | no | Instead of the App: issue read/write on the configured repository. Without either, the caller's `GITHUB_TOKEN` is used |
| `tool-token` | no | Instead of the App: read access to `cccteam/gh-jira-sync`, for callers in other repositories |
| `project-token` | no | Instead of the App: Projects read/write, for example for a user-owned project |

An explicitly passed `github-token`, `tool-token`, or `project-token` takes precedence
over the matching App token.

The calling workflow must grant `contents: write` (for the state branch) and
`issues: write`, and branch rules must allow pushes to the state branch. The
reusable job runs only on the default branch, so secret-bearing code never runs
from pull requests or other branches. It serializes runs per repository and state
branch and never cancels an active run.

### GitHub App (recommended)

One GitHub App can supply every GitHub token, so no personal access tokens are
needed. Each run mints a separate installation token per purpose, each limited to
one repository or owner and only the permission that purpose needs. They expire
within an hour, are revoked when the job ends, and act as the App rather than a
person (copied comments show the App as their author).

| Token | Minted when | Scope |
| --- | --- | --- |
| Tool checkout | the caller is not `cccteam/gh-jira-sync` | Contents: read, on `cccteam/gh-jira-sync` only |
| Issues and comments | always | Issues: read and write, on the config's `repository` only |
| Projects | the config has `project_status` | Organization Projects: read and write, for `project_status.owner` |

The workflow reads `repository` and `project_status.owner` from the config, so the
same App works for any repository it is installed on. The calling repository's own
`GITHUB_TOKEN` still pushes the state branch, so App tokens never need write access
to code.

Set it up once per organization:

1. **Settings → Developer settings → GitHub Apps → New GitHub App**, owned by the
   organization. Turn off the webhook. Repository permissions: **Contents: Read**,
   **Issues: Read and write** (Metadata: Read is added automatically). Organization
   permissions: **Projects: Read and write** if you use `project_status`.
2. Install it on the organization for **only** `cccteam/gh-jira-sync` and the
   repositories being synced.
3. Note its **Client ID** and generate a private key. Store the key as an
   organization secret visible only to the repositories that run the sync, or as an
   environment secret whose deployment branch policy allows only the default branch.
4. Pass both to the workflow:

```yaml
    with:
      app-client-id: Iv23liExampleClientId
    secrets:
      app-private-key: ${{ secrets.GH_JIRA_SYNC_APP_KEY }}
```

### This repository's deployment

[`sync.yml`](.github/workflows/sync.yml) calls the reusable workflow with
`sync.json`, the `sync-state` branch, and the tool built from the same commit.

1. Add Actions secrets `JIRA_EMAIL` and `JIRA_API_TOKEN`, and commit `sync.json`.
2. Dispatch **Sync GitHub and Jira** with **initialize** checked and **apply**
   unchecked. Inspect the job summary.
3. Dispatch with initialize and apply checked for the first write run. Later runs
   need only apply. Run again to copy comments for newly created Jira issues.
4. Hourly runs remain dry-run until repository variable **`SYNC_APPLY=true`** is set.

### Another repository syncing itself

Commit a `sync.json` whose `repository` is that repository, add the Jira secrets
and the App's private key, then add a workflow like this. Pin `uses:` and `tool-ref` to the
same tag or commit so the workflow and the tool always match:

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
  issues: write
jobs:
  sync:
    uses: cccteam/gh-jira-sync/.github/workflows/sync-reusable.yml@<commit-or-tag>
    with:
      tool-ref: <commit-or-tag>
      app-client-id: Iv23liExampleClientId
      apply: ${{ inputs.apply || (github.event_name == 'schedule' && vars.SYNC_APPLY == 'true') }}
      initialize: ${{ inputs.initialize || false }}
    secrets:
      jira-email: ${{ secrets.JIRA_EMAIL }}
      jira-api-token: ${{ secrets.JIRA_API_TOKEN }}
      app-private-key: ${{ secrets.GH_JIRA_SYNC_APP_KEY }}
```

While `cccteam/gh-jira-sync` is private, other repositories can call its workflow
only if its **Settings → Actions → General → Access** allows repositories in the
organization. The App mints the read-only token that checks out its code.

### One repository serving several

A single hub repository can run several deployments. Give each its own config file
and state branch. With the App installed on every target repository, each
deployment gets an issues token for its own config's repository; the hub's
`GITHUB_TOKEN` would only cover the hub itself:

```yaml
jobs:
  sync:
    strategy:
      fail-fast: false
      matrix:
        deployment: [web, api]
    uses: cccteam/gh-jira-sync/.github/workflows/sync-reusable.yml@<commit-or-tag>
    with:
      tool-ref: <commit-or-tag>
      config: sync/${{ matrix.deployment }}.json
      state-branch: sync-state-${{ matrix.deployment }}
      app-client-id: Iv23liExampleClientId
      apply: ${{ inputs.apply || (github.event_name == 'schedule' && vars.SYNC_APPLY == 'true') }}
      initialize: ${{ inputs.initialize || false }}
    secrets:
      jira-email: ${{ secrets.JIRA_EMAIL }}
      jira-api-token: ${{ secrets.JIRA_API_TOKEN }}
      app-private-key: ${{ secrets.GH_JIRA_SYNC_APP_KEY }}
```

Deployments are independent: one failing or blocked does not stop the others, and
each state branch is bound to its config's repository, Jira site, and project, so a
config pointed at the wrong branch fails closed. State branches use a dash
(`sync-state-web`), because Git cannot hold both `sync-state` and `sync-state/web`.
A state branch must contain only `state.json`; the tool refuses any other branch,
since each checkpoint replaces the branch's whole tree.

The workflow loads authoritative state from `origin`, pushes every creation intent
**before** the corresponding API request, and checkpoints each success. Use exactly
one deployment per GitHub repository and Jira project pair: two deployments writing
the same objects keep separate journals and can duplicate issues and comments. The
test workflow can run on pull requests without sync credentials.

## Configuration

See [sync.example.json](sync.example.json). Unknown keys fail validation.

| Option | Default / meaning |
| --- | --- |
| `repository` | Required `owner/name` |
| `jira_url` | Required Jira Cloud HTTPS site URL |
| `jira_project` | Required project key, e.g. `ENG` |
| `jira_issue_type` | Required **issue type ID string**, e.g. `10001` |
| `jira_issue_types` | `{}`; maps GitHub issue types to Jira issue type IDs for new issues, e.g. `{"Bug": "10004", "Feature": "10005"}`; see [Issue types](#issue-types) |
| `github_url` | `https://api.github.com`; compatible custom API bases allowed |
| `selection_label` | `sync-jira`; `null` explicitly opts into all issues |
| `issue_state` | `open`; `closed` or `all` also supported for creation selection |
| `comments` | `true`; copy new comments and original edits |
| `comments_since` | `null` backfills existing comments; ISO timestamp with timezone limits initial enrollment |
| `max_operations` | `20` remote writes per run; extra writes are reported as `deferred` and planned again next run |
| `max_pages` | `100` pages per collection; incomplete scans fail closed |
| `full_rescan_hours` | `24`; maximum time an unchanged pair goes without a full comment scan |
| `jira_create_fields` | Extra Jira create fields, such as required custom fields or priority |
| `status_sync` | `null` (off); `{"close_transition": "Done", "reopen_transition": "Reopen"}` mirrors closing and reopening |
| `project_status` | `null` (off); mirrors a GitHub Project's Status field and Jira statuses instead of `status_sync`; see [Project status](#project-status) |

`jira_create_fields` cannot replace project, issue type, title, or description. It
may set `labels`; the tool adds none of its own. Required tenant-specific fields must be supplied explicitly;
the tool does not guess them or truncate long titles. `comments_since` applies to
newly enrolled comments, not edits to already tracked comments.

The label/state filter selects **new issue creation**. Once a pair is tracked,
comment sync continues even if its GitHub issue closes or loses the label. Set
`comments=false` to pause comment synchronization globally. If either issue of a
tracked pair is deleted, transferred, moved to another Jira project, or no longer
visible, that pair is reported as `unavailable` and its comment sync pauses; other
pairs continue.

To keep API usage flat as pairs accumulate, a pair whose full comment scan found
nothing to do is recorded as `reconciled` with its GitHub `updated_at` and Jira
`updated` timestamps. Later runs report it as `unchanged` and skip both comment
lists until either timestamp changes, an operation for it is not `done`, or
`full_rescan_hours` passes. New comments bump both timestamps, so they sync on the
next run. If a service does not bump the issue timestamp for a comment edit, that
edit waits for the periodic rescan. Pairs with anything blocked, unavailable, or
acknowledged are always fully scanned. Existing Jira issues
are not matched by title. Only issues with this utility's verified source identity
can be adopted during recovery.

CLI options: `--state` (default `.sync-state/state.json`), `--report` (default
`sync-report.json`), `--git-state-branch sync-state` (or `sync-state-<name>`), `--init-state`, `--apply`, and
`--acknowledge OPERATION` (repeatable). The Git option is required for ephemeral runners.

`--acknowledge` takes an `operation` from a `blocked` or `unavailable` report entry
after someone has reviewed it. The entry is then reported as `acknowledged` and no
longer causes exit code 2. It is saved only with `--apply`, and it lapses
automatically if that entry's reason changes or the entry resolves, so a new
problem with the same object alerts again. Acknowledging never permits a retry. In that mode the branch is
authoritative, and the local file is only a checkpoint copy. Dry-run creates a
report/lock file but does not modify state or either service. Apply computes a
fresh plan; a previous report is not executable authorization.

## Issue types

By default every Jira issue is created with `jira_issue_type`. To follow GitHub's
organization issue types instead, map them:

```json
"jira_issue_type": "10001",
"jira_issue_types": {"Bug": "10004", "Feature": "10005", "Task": "10001"}
```

Names match GitHub's issue type names, ignoring case. An issue with no type, or a
type missing from the map, uses `jira_issue_type`. Every configured ID must be a
standard (non-subtask) issue type in the Jira project; otherwise the run fails
before doing anything. The type is chosen when the Jira issue is created and is not
synced afterwards. Dry runs show the chosen type as `issue_type` on `create_issue`
entries. List a project's issue type IDs with:

```sh
curl -s -u "$JIRA_EMAIL:$JIRA_API_TOKEN" "$JIRA_SITE/rest/api/3/project/<KEY>" | jq '.issueTypes[] | {id, name, subtask}'
```

## Issue status

With `status_sync` set, each tracked pair's open/closed status is mirrored in both
directions. A Jira status counts as closed when it is in Jira's **Done** category
(for example Done or Won't Do), so the mapping works with any workflow.

* **GitHub → Jira:** closing a GitHub issue applies the Jira transition named
  `close_transition`; reopening applies `reopen_transition`. Names must match the
  workflow's transition names exactly. If that transition is not available from
  the issue's current status, or leads to the wrong kind of status, the issue is
  blocked for review and nothing changes.
* **Jira → GitHub:** moving into a Done-category status closes the GitHub issue as
  completed; moving out of it reopens the issue.

Each pair records the status both sides last agreed on. The side that now differs
from it is the side that changed, and the other side is updated to match. If both
sides changed, they agree again and nothing is written. The one ambiguous case is a
pair whose sides disagree and that has no agreed status yet, for example pairs
created before status sync was turned on. It is blocked for review: align the two
sides by hand (or acknowledge it), and the agreement is recorded on the next run.
Issues created while status sync is on start with "open" recorded, so a GitHub issue
that was already closed closes its new Jira issue on the following run.

Status changes count toward `max_operations`, are re-checked immediately before
and after each write, and are not journaled: they are idempotent, so a lost response
simply shows up as agreement on the next run. A refused transition (for example one
whose screen requires fields) is reported as `rejected` and retried next run. Status
is checked every run, even when comment sync is off or a pair's comments are
skipped as unchanged.

## Project status

`project_status` keeps a GitHub Project's single-select field (normally **Status**)
and the Jira issue's workflow status in step, in both directions. It replaces
`status_sync`; a config cannot use both.

```json
"project_status": {
  "owner": "cccteam",
  "project_number": 7,
  "map": [
    {"project": "Todo",        "jira": ["To Do", "Backlog"]},
    {"project": "In Progress", "jira": ["In Progress", "In Review"]},
    {"project": "Done",        "jira": ["Done", "Won't Do"]}
  ]
}
```

| Key | Default | Meaning |
| --- | --- | --- |
| `owner` | required | Organization or user that owns the project |
| `owner_type` | `org` | `org` or `user` |
| `project_number` | required | The number in the project's URL |
| `field` | `Status` | Name of the single-select field to sync |
| `add_to_project` | `true` | Add linked issues that are not in the project yet; otherwise report them as `excluded` |
| `map` | required | Each project option and the Jira statuses it covers (a name or a list) |
| `map[].transition` | none | Jira transition to use when several lead to the target status |

How it behaves:

* **Mapping.** Each Jira status belongs to at most one option. When several Jira
  statuses share an option, the first listed is where Jira moves when the project
  changes; Jira moving between them leaves the project alone.
* **Project → Jira.** Jira takes the transition whose destination is the target
  status (or the named `transition`). If none is available from the current status,
  the issue is blocked; multi-step paths are not taken.
* **Jira → project.** The item's field is set to the option covering Jira's status.
* **Which side changed.** Each pair records the option both sides last agreed on;
  the side that differs from it changed. If both changed to different values, or
  they disagree with nothing recorded yet, the issue is blocked for review. An item
  with an empty status is filled from Jira.
* **Not mapped.** A Jira status or project option missing from `map` blocks that
  issue. A mapped option that the project does not have fails the whole run, since
  it is a configuration error.
* **Open/closed.** Jira's Done category still closes and reopens the GitHub issue.
  Closing or reopening on GitHub does not move Jira directly: turn on the project's
  built-in workflows **Item closed** (set Done) and **Item reopened**, and the status
  change reaches Jira through project sync.
* **Safety.** Like other writes, adding items and status changes count toward
  `max_operations`, are re-checked before and after, and are retried next run if
  refused. If the re-check finds both sides already at the target (for example
  because the project's "Item closed" workflow moved the card after the tool closed
  the issue), the entry is reported as `already_done` and the agreement is recorded.

### Credentials for projects

Workflow tokens cannot reach organization projects, so project calls use a separate
token in `GH_PROJECT_TOKEN`; issues and comments use `GH_TOKEN`. In Actions, the
[GitHub App](#github-app-recommended) mints it with organization Projects permission;
for a user-owned project, pass a fine-grained token as `project-token` instead.

For local runs, prefer a fine-grained personal access token with an expiry, limited
to the synced repository (Issues: read and write) and organization Projects (read and
write), over a broad `gh auth token`. Export it as both `GH_TOKEN` and
`GH_PROJECT_TOKEN`.

## Comments and formatting

Copies start with a plain header naming the original author and linking to the
original, for example "alice wrote on GitHub (link):" in Jira, or "**Bob** wrote
in [Jira](link):" on GitHub. The actual API author is the service account.

Each copy also carries an invisible identity that prevents echo loops and lets a
lost response be recovered: a comment property on Jira, and an HTML comment
(`<!-- gh-jira-sync:… -->`) on GitHub, which GitHub does not display. Two identical
originals still produce two distinct copies. Quote-replies of a copy sync as new
originals, because a quote never carries the identity. Do not remove the header
from a copy, or the HTML comment when editing a GitHub copy's Markdown; either
blocks that comment for review. Copies made by older releases end with a visible
`[gh-jira-sync:…]` line and keep working.

GitHub Markdown is preserved literally in Jira, with CRLF line endings from the
GitHub web editor normalized to LF. Jira comments are rendered as
readable plain text with links and list text; rich visual formatting is not kept.
Unsupported media/tables block that comment for review. No attachments are uploaded.
Restricted Jira comments are excluded. A comment's later deletion or loss of access
is reported without deleting or recreating its counterpart. If visibility becomes
restricted after export, the existing copy remains under the no-delete policy and
requires manual review.

## Reading the report

Every run writes the report to `--report` (default `sync-report.json`) and prints it;
in Actions it appears in the job summary. It holds identifiers and decisions, never
comment or issue text.

```json
{
  "mode": "apply",
  "planned_writes": 2,
  "deferred_writes": 0,
  "exit_code": 2,
  "entries": [
    {"github": 12, "operation": "3f9c…", "kind": "create_comment",
     "source": "github", "comment_id": "2210", "destination": "jira"},
    {"github": 12, "operation": "a71e…", "kind": "sync_status",
     "destination": "jira", "status": "closed", "outcome": "rejected",
     "reason": "Status change rejected with HTTP 400; retried next run"},
    {"github": 15, "operation": "c04d…", "kind": "unavailable",
     "reason": "Linked issue deleted, transferred, moved, or inaccessible; comment sync paused"}
  ]
}
```

### Report fields

| Field | Meaning |
| --- | --- |
| `mode` | `dry-run` or `apply` |
| `entries` | One decision per issue, comment pair, or comment, in processing order; empty if the run failed before planning |
| `planned_writes` | Writes planned for this run, after deferral; absent if planning failed |
| `deferred_writes` | Writes pushed to a later run by `max_operations`; absent if planning failed |
| `error` | Present only when the run failed (exit code 1); never includes response bodies |
| `exit_code` | `0`, `1`, or `2`; see [exit codes](#safety-recovery-and-limits) |

### Entry fields

Every entry has `github`, `operation`, and `kind`; the rest appear only when they apply.

| Field | Meaning |
| --- | --- |
| `github` | GitHub issue number the entry belongs to |
| `operation` | Stable 64-character ID of the decision; pass it to `--acknowledge` |
| `kind` | The decision; see the table below |
| `reason` | Why an entry is blocked, unavailable, excluded, deferred, unchanged, or rejected (kept on `acknowledged`) |
| `source` | Side of the original comment: `github` or `jira` |
| `comment_id` | ID of that original comment on its own side |
| `destination` | Side that receives the write: `github` or `jira` for comment copies and status changes, or `project` for the GitHub Project item |
| `status` | Target: `open` or `closed` for `sync_status` and `status_agreed`; the project option for `sync_project_status` and `project_status_agreed` |
| `previous_rejection` | HTTP status of an earlier refused attempt that this entry retries |
| `issue_type` | Jira issue type ID a `create_issue` entry will use |
| `outcome` | Set during apply: `rejected` when the service refused the write (nothing changed; retried next run), or `already_done` when a status change was already in place, for example made by a project workflow, so nothing was written and the agreement was recorded |

Entries are planned before any write. In apply mode, only `outcome` and `reason` can
change afterwards. If a run fails part way (exit code 1), entries before the failure
may already be done; rerun a dry-run to see what remains.

### Entry kinds

Kinds marked **write** change GitHub or Jira and count toward `max_operations`. The
others write nothing to either service; some only update the state journal.

| Kind | Type | Meaning |
| --- | --- | --- |
| `create_issue` | write | Create a Jira issue for a newly selected GitHub issue |
| `link_issue` | state | A Jira issue with this GitHub issue's identity already exists (for example after a lost response); record the pair instead of creating another |
| `create_comment` | write | Copy an original comment to the other side |
| `recover_comment` | state | A copy from an interrupted earlier run exists; record it instead of copying again |
| `edit_comment` | write | The original comment was edited; update its copy (`destination`) |
| `confirm_comment` | state | Both sides already show the new text (for example an edit whose response was lost); re-read and record it |
| `sync_status` | write | Close or reopen the `destination` side to match the other (`status`) |
| `status_agreed` | state | Both sides already have the same status; record it as the agreed status |
| `add_to_project` | write | Add the GitHub issue to the project; its status is synced on the next run |
| `sync_project_status` | write | Set the project item's option, or transition Jira, to match the other side (`destination`, `status`) |
| `project_status_agreed` | state | The project option and Jira status already match; record it as the agreed option |
| `noop` | none | A tracked comment pair is in sync |
| `reconciled` | state | Every comment on this pair is in sync; record the issues' timestamps so later runs can skip reading its comments |
| `unchanged` | none | Neither issue has changed since the last full scan; its comments were not read this run |
| `excluded` | none | A comment is intentionally not copied: a restricted Jira comment, or older than `comments_since` |
| `deferred` | none | A write over the `max_operations` budget; planned again next run |
| `blocked` | review | Needs a person; `reason` says why. Causes exit code 2 |
| `unavailable` | review | An issue or comment was deleted, moved, or became inaccessible; nothing is deleted or recreated. Causes exit code 2 |
| `acknowledged` | none | A `blocked` or `unavailable` entry someone acknowledged; it stays quiet while its reason is unchanged |

To handle a `blocked` or `unavailable` entry, read its `reason`, fix the cause if
there is one, and either let the next run resolve it or acknowledge it:

```sh
./gh-jira-sync --config sync.json --apply --acknowledge <operation>
```

Acknowledgements are saved only with `--apply`, so that run also performs any other
planned writes; run a dry-run first if that matters. The workflow has no acknowledge
input yet. For an Actions deployment, run the command from a clone of the repository
that holds the state branch, adding `--git-state-branch <branch>` and the same
credentials the workflow uses.

## Safety, recovery, and limits

This aims for repeatable, duplicate-resistant synchronization, not a promise of
zero risk. APIs do not provide a cross-service transaction. Final read/write races,
notifications, Jira automation, and incompatible project audiences remain possible.
Pilot in a sandbox and review the first plans.

A creation journal records `pending` before dispatch and `done` after confirmation.
Writes are never retried within a run. If a response is lost, the next run looks for
the destination by its hidden identity (for issues: issues this account created
since the intent was journaled, matched by issue property) and adopts a match. If no result is visible,
it **blocks instead of creating another**. Jira search can lag behind writes.

A 4xx answer (other than 408) proves the service refused the write, so the journal
records `rejected` instead of `pending`, and the next run plans it again with
`previous_rejection` in the report. Item-specific refusals (400, 404, 409, 413, 422),
such as a missing required Jira field, are reported with `outcome: rejected` and
exit code 2 while the rest of the run continues. Refusals that affect every write
(401, 403, 429) stop the run. Fix the cause; no state editing is needed.

For a pending operation (timeout, network failure, 5xx, or lost response):

1. Restore/retain the newest state; rerun dry-run after allowing indexing to catch up.
2. If the destination exists with intact markers, apply can recover its mapping.
3. If the request definitely never created anything, have an operator remove
   **only that pending operation** from `state.json` after verifying the destination
   and coordinating all writers. In Actions, make the reviewed change on the deployment's state branch.
   Do not delete completed mappings or reset the entire journal.
4. If the outcome cannot be proven, leave it blocked and investigate manually.

Do not initialize over lost production state or restore an old checkpoint casually:
that may erase pending intents and invalidate duplicate protection. State binds the
repository, service endpoints, and Jira project. Changing those needs a separate,
reviewed deployment. The old experimental field-sync state format is not compatible.

State contains object IDs, attribution metadata, source URLs, envelopes, and content
fingerprints, but no source comment bodies or tokens. Treat it as private operational
metadata. Protect its branch and the workflow/config from untrusted edits. Reports
contain IDs and decisions, not comment bodies. Markers prevent accidental loops;
they are not a security boundary against users deliberately forging sync metadata.

Exit codes:

* `0`: successful plan/apply, including runs with `deferred` writes.
* `2`: unacknowledged blocked operations, deleted/inaccessible comments or issues, or
  item-specific write rejections requiring review.
* `1`: configuration, API, pagination, concurrency, or persistence error.

Independent operations may complete before a failure or alongside blocked comments.
`planned_writes` counts intended writes, not confirmed completions; after apply, each
entry's `outcome` shows a refusal. Read the error and rerun dry-run to inspect
remaining work. Remote state failures stop execution; no force push occurs.

Tests use fake APIs plus local Git integration. Live authentication, required Jira
fields, branch protection, and actual workflow execution remain to be validated
with your sandbox credentials.
