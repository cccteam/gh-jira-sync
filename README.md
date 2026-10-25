# GitHub → Jira creation, two-way comments

Creates Jira Cloud issues from GitHub issues labeled **`sync-jira`**, then copies
original comments and their edits in both directions. A single Go binary (Go 1.23+,
standard library only) with a ready-to-use GitHub Actions workflow.

* Issue title and description are copied at creation only.
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
Comments, and access to issue properties. Jira Software and business projects are
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

`go install .` also works. Flags accept one or two dashes (`-apply` or `--apply`).
Supported platforms are Linux/macOS (the local lock uses `flock`). No live
credentials are needed to run tests.

State, report, and identity formats are unchanged from the earlier Python release:
operation tokens, Jira `gh-sync-*` labels, and content fingerprints are byte-identical
(checked against vectors in `internal/ghjira/testdata/golden.json`), so existing
`sync-state` branches and Jira issues keep working.

## GitHub Actions

Commit the code, workflows, and your `sync.json` to the default branch of the
repository being synced.

1. Add Actions secrets `JIRA_EMAIL` and `JIRA_API_TOKEN`.
2. Permit the workflow's `GITHUB_TOKEN` to write issues and contents. Contents write
   is needed for the dedicated `sync-state` branch; branch rules must allow it.
3. Dispatch **Sync GitHub and Jira** on the default branch with **initialize** checked
   and **apply** unchecked. Inspect the job summary.
4. Dispatch with initialize and apply checked for the first write run. Later runs
   need only apply. Run again to copy comments for newly created Jira issues.
5. Hourly runs remain dry-run until repository variable **`SYNC_APPLY=true`** is set.

The workflow uses `--git-state-branch sync-state`: it loads authoritative state
from `origin`, pushes every creation intent **before** the corresponding API request,
and checkpoints each success. It uses a fixed concurrency group and never cancels
an active run. It does not run secret-bearing code from pull requests or non-default
branches. The test workflow can run on pull requests without sync credentials.

Use one writer deployment for a repository/project. Do not run separate local
journals or other repositories' workflows against the same objects. A different
source repository needs a scoped GitHub App token or PAT in `GH_TOKEN`; the default
`GITHUB_TOKEN` only covers the workflow's repository. State writes still use the
checkout's Git credentials for the workflow repository.

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
`sync-report.json`), `--git-state-branch sync-state`, `--init-state`, `--apply`, and
`--acknowledge OPERATION` (repeatable). The Git option is required for ephemeral runners.

`--acknowledge` takes an `operation` from a `blocked` or `unavailable` report entry
after someone has reviewed it. The entry is then reported as `acknowledged` and no
longer causes exit code 2. It is saved only with `--apply`, and it lapses
automatically if that entry's reason changes or the entry resolves, so a new
problem with the same object alerts again. Acknowledging never permits a retry. In that mode the branch is
authoritative, and the local file is only a checkpoint copy. Dry-run creates a
report/lock file but does not modify state or either service. Apply computes a
fresh plan; a previous report is not executable authorization.

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
   and coordinating all writers. In Actions, make the reviewed change on `sync-state`.
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
