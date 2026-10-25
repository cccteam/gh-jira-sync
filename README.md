# GitHub → Jira creation, two-way comments and status

Creates Jira Cloud issues from GitHub issues labeled **`sync-jira`**, then copies
original comments and their edits in both directions. A single Go binary (Go 1.27+,
standard library only) with a ready-to-use GitHub Actions workflow.

* Issue title and description are copied at creation only.
* Optionally, closing or reopening an issue on either side does the same on the
  other (see [Issue status](#issue-status)).
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

State, report, and identity formats are unchanged from the earlier Python release:
operation tokens, Jira `gh-sync-*` labels, and content fingerprints are byte-identical
(checked against vectors in `internal/ghjira/testdata/golden.json`), so existing
`sync-state` branches and Jira issues keep working.

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

| Secret | Required | Purpose |
| --- | --- | --- |
| `jira-email`, `jira-api-token` | yes | Jira Cloud service account |
| `github-token` | no | Issue read/write on the configured repository; defaults to the caller's `GITHUB_TOKEN` |
| `tool-token` | no | Read access to `cccteam/gh-jira-sync`, needed by other repositories while it is private |

The calling workflow must grant `contents: write` (for the state branch) and
`issues: write`, and branch rules must allow pushes to the state branch. The
reusable job runs only on the default branch, so secret-bearing code never runs
from pull requests or other branches. It serializes runs per repository and state
branch and never cancels an active run.

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
and a `tool-token`, then add a workflow like this. Pin `uses:` and `tool-ref` to the
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
      apply: ${{ inputs.apply || (github.event_name == 'schedule' && vars.SYNC_APPLY == 'true') }}
      initialize: ${{ inputs.initialize || false }}
    secrets:
      jira-email: ${{ secrets.JIRA_EMAIL }}
      jira-api-token: ${{ secrets.JIRA_API_TOKEN }}
      tool-token: ${{ secrets.GH_JIRA_SYNC_READ_TOKEN }}
```

While `cccteam/gh-jira-sync` is private, other repositories can call its workflow
only if its **Settings → Actions → General → Access** allows repositories in the
organization, and its code checkout needs `tool-token`: a fine-grained token or
GitHub App token with read-only Contents access to this repository.

### One repository serving several

A single hub repository can run several deployments. Give each its own config file
and state branch, and a `github-token` that can read and write issues in its target
repository (the default `GITHUB_TOKEN` only covers the hub itself):

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
      apply: ${{ inputs.apply || (github.event_name == 'schedule' && vars.SYNC_APPLY == 'true') }}
      initialize: ${{ inputs.initialize || false }}
    secrets:
      jira-email: ${{ secrets.JIRA_EMAIL }}
      jira-api-token: ${{ secrets.JIRA_API_TOKEN }}
      github-token: ${{ secrets.ISSUES_TOKEN }}
      tool-token: ${{ secrets.GH_JIRA_SYNC_READ_TOKEN }}
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

`jira_create_fields` cannot replace project, issue type, title, description, or
identity labels. Required tenant-specific fields must be supplied explicitly;
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

## Comments and formatting

Copies identify the original author, creation time, and source URL. The actual API
author is the service account. Stable source IDs and markers prevent echo loops;
two identical original comments still produce two distinct copies. A comment is
treated as a copy only when the marker is its own last line (in Jira, its last
paragraph), so quote-replies of a copy sync as new originals. Do not manually
remove or alter attribution or `[gh-jira-sync:…]` markers.

GitHub Markdown is preserved literally in Jira, with CRLF line endings from the
GitHub web editor normalized to LF. Jira comments are rendered as
readable plain text with links and list text; rich visual formatting is not kept.
Unsupported media/tables block that comment for review. No attachments are uploaded.
Restricted Jira comments are excluded. A comment's later deletion or loss of access
is reported without deleting or recreating its counterpart. If visibility becomes
restricted after export, the existing copy remains under the no-delete policy and
requires manual review.

## Safety, recovery, and limits

This aims for repeatable, duplicate-resistant synchronization, not a promise of
zero risk. APIs do not provide a cross-service transaction. Final read/write races,
notifications, Jira automation, and incompatible project audiences remain possible.
Pilot in a sandbox and review the first plans.

A creation journal records `pending` before dispatch and `done` after confirmation.
Writes are never retried within a run. If a response is lost, the next run searches
for the destination marker and adopts a matching result. If no result is visible,
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
