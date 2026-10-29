# GitHub ↔ Jira issues, comments, status, and parents

## Agreed scope

* Jira Cloud, initially Software and business projects.
* Create a Jira issue for each eligible GitHub issue labeled `sync-jira`.
* Copy title and description at creation, choosing the Jira issue type from the
  GitHub issue type when `jira_issue_types` maps it. Do not subsequently sync issue fields,
  labels, assignees, or deletions.
* Optionally create GitHub issues from Jira issues with `jira_selection_label`.
* Optionally mirror open/closed status in both directions (`status_sync`).
* Optionally mirror parent links: GitHub sub-issues and Jira parents (`sync_parents`).
* Or mirror a GitHub Project's Status field and Jira statuses (`project_status`).
* Copy original comments in both directions, including later edits to originals.
* Report edits to copied comments; preserve them for human review.
* Never delete comments or recreate a known deleted copy.

The earlier idea of bidirectional issue-field sync is superseded. Jira-wins field
conflict resolution no longer applies: issue fields are creation-only, status
follows whichever side changed, and a comment's original remains its authority.

## Issue status

GitHub has two states; Jira has per-project workflows. They meet at Jira's status
categories: a Done-category status is closed, anything else is open. Jira → GitHub
needs no configuration. GitHub → Jira uses two transitions named in `status_sync`,
because a workflow can have several ways into or out of Done and guessing could
choose the wrong one. The named transition must be available and must land in the
expected category, or the pair is blocked.

Each pair stores the last agreed status (`closed` on the pair, absent until first
agreement). Comparing each side with it identifies which side moved. Two states make
true conflicts impossible: if both moved, both now agree. Only disagreement with no
agreed status is ambiguous, and it blocks rather than guessing a winner. Status
writes are idempotent, so unlike creates they are not journaled; they are verified
before and after the write and confirmed by recording the new agreed status. A
re-check that finds the target already reached (another actor, such as a project
workflow reacting to an earlier write in the same run, made it) records the
agreement as `already_done` instead of failing the run.

## Architecture

The code is one Go package, `internal/ghjira`, with a thin `main.go`:

* `core.go` validates configuration, derives stable identities, and converts text.
* `canonical.go` produces the canonical JSON behind digests and tokens. It matches
  Python's `json.dumps(sort_keys=True, ensure_ascii=False)` byte for byte, so state
  and Jira labels from the earlier Python release stay valid.
* `api.go` handles paginated GitHub/Jira REST reads and narrowly scoped writes.
* `engine.go` separates planning from execution and manages identity/recovery.
* `storage.go` defines the state types, local checkpoints, and the Git-branch store.
* `cli.go` provides dry-run/apply, locking, reports, and nonzero failure codes.
* `markdown.go` converts GitHub Markdown to Jira rich text for new issue descriptions.
* `version.go` reports the release a binary was built from.
* `types.go` defines named types for every closed set of values (`Side`, `Kind`,
  `Status`, `OpKind`, `Outcome`) and the GitHub/Jira response shapes.

GitHub and Jira responses decode into typed structs, so a field with an unexpected
type fails the request instead of reading as an empty value. Both services' comments
become one `Comment` view (ID, author, creation time, restriction, body), and
comparisons before writing use that view, so reactions or activity timestamps never
abort a run. Three things stay generic JSON on purpose:

* Comment bodies (`Comment.Body`): a Markdown string on GitHub, an ADF document on
  Jira. Copy fingerprints hash the body exactly as received (numbers as
  `json.Number`), which keeps them identical to the Python release.
* The ADF tree itself, which `adfText` walks and rejects when unsupported.
* `jira_create_fields`, which holds tenant-specific Jira fields.

State is strictly typed: unknown keys and unknown enum values (status, operation
kind, origin side) are rejected when it is loaded.

No runtime dependencies beyond the Go standard library; Linux/macOS. Git is needed for remote
state persistence. Scheduled reconciliation avoids operating a public webhook
service and naturally catches missed events.

## Identity and creation

An operation token hashes the source service/repository/project identity, object
kind, and immutable source object ID. GitHub issue numbers are used for API routing;
immutable IDs detect transfers/replacements. The created Jira issue carries its identity
only in an issue property set in the same creation request; nothing visible is added.
State records its Jira ID and key, and each journaled intent records when it started.
Recovery runs only for pending intents: it searches issues this account created in
the project since the intent started (with a margin for clock skew), returns their
properties in the same search, and adopts the one whose property matches exactly.
Multiple matches or a mismatched identity block adoption, and no match leaves the
intent blocked, never retried. Intents journaled before start times were recorded
fall back to the `gh-sync-<token>` label older releases added. Because state is
authoritative, issues are not searched for otherwise; existing Jira issues cannot be
re-adopted after state is lost.

Before every POST, persist a `pending` operation. In Actions this means successfully
pushing a unique commit to the deployment's state branch on `origin`
(`sync-state` or `sync-state-<name>`), not just writing runner-local disk. The
store refuses a branch whose tree holds anything but `state.json`, because each
checkpoint replaces the whole tree.
After confirmed success, persist the destination mapping and mark it `done`.

If the response or completion checkpoint is lost, a later run checks destination
identity. A visible matching object is adopted. If none is visible, leave the
operation pending and report it. Jira search is eventually consistent: absence is
not proof a POST failed. There is deliberately no blind retry of an uncertain
creation. This trades availability for duplicate safety. A 4xx answer (except 408)
is not uncertain: the service refused the write, so the intent is recorded as
`rejected` and planned again on the next run. Refusals about one item (400, 404,
409, 413, 422) let the run continue; others (401, 403, 429) stop it.

Creating an issue and copying its comments are separate reconciliation passes.
This keeps the full current-pass write budget reviewable before any writes begin.

## Comment ownership and edits

Each copied comment carries a readable attribution header with the original link,
and an invisible operation identity: a Jira comment property set atomically on
create (read back through the bulk `comment/list` endpoint, which documents
expanding properties), or a trailing HTML comment on GitHub. Older copies end with
a visible marker line instead; both are recognized. The journal records original/copy IDs, origin side, envelope,
last confirmed content fingerprint, and raw copy-body fingerprint. Known copied
IDs and markers prevent echoes. A marker counts only as the body's final line (or
final top-level ADF paragraph), so quoted copies inside replies stay originals.
Identical text in different original comments remains two distinct comments.
GitHub CRLF line endings are normalized to LF before hashing and conversion.

| Original | Copy | Action |
| --- | --- | --- |
| Unchanged | Unchanged | No API write or re-read |
| Edited | Matches baseline | Update existing copy |
| Either | Edited independently | Report; no overwrite |
| Same new value | Matches expected generated body | Confirm recovered edit |
| Removed/inaccessible | Either | Report; no delete or recreation |

Both comments are read again immediately before an edit and after it; only fields
that drive sync decisions are compared, so reactions or counters changing mid-run
do not abort it. A pending copy whose recovered content does not match is blocked
at plan time, affecting only that comment. Checkpoints
advance only after agreement. Copy fingerprints also detect formatting-only edits.
No API operation edits an original comment. Attribution remains the original author;
API authorship is the service account, not impersonation.

Issue descriptions are converted from GitHub Markdown to rich ADF (`markdownADF`, a
small standard-library parser for the constructs issue bodies use). Nothing reads a
description back for comparison, so rich formatting there costs nothing. Comment
copies stay literal: their edit detection compares the copy's ADF with the expected
rendering of its text, so a rich conversion would also have to round-trip through
`adfText` exactly, and existing copies would all look edited.

In comment copies, GitHub Markdown is copied literally into Jira ADF paragraphs. Jira text, paragraphs,
headings, lists, links, quotes, code, and common formatting are exported as readable
plain text. Unsupported structures such as media/tables block that comment instead
of silently dropping content. Restricted Jira comments are excluded. Jira Service
Management projects are rejected because internal/public comment handling needs a
separate integration. Visibility changes after export require manual review; the
no-delete rule means previously exported content is not automatically removed.

## Persistence and concurrency

State version 2 binds endpoints, repository, and Jira project. It includes mappings,
creation intents, attribution metadata, and fingerprints, but no source comment
bodies or credentials. Git state is authoritative on ephemeral runners; local state
alone is suitable only with durable disk. Corrupt or missing state fails closed
unless a new deployment is explicitly initialized.

Git commits have unique IDs even for identical payloads, so competing writers
cannot both treat the same pending-intent commit as their own authorization to
POST. Pushes never force or retry. The workflow additionally serializes all runs
using a concurrency group per repository and state branch, and disables active
cancellation.
Use one deployment/journal per pair population; different repositories or journals
are not covered by this lock. Local execution uses an exclusive advisory file lock.

## Failure and operational policy

Dry-run is the default and reports identifiers/decisions, not comment contents.
Apply recomputes a fresh plan. Pagination must finish before any writes; page limits
fail the whole plan. Writes beyond the operation budget are marked `deferred` in plan
order and replanned on the next run, so a large backfill progresses in bounded,
reviewable batches. A tracked pair whose GitHub or Jira issue is deleted, transferred,
moved, or inaccessible is reported `unavailable` without stopping other pairs.
A pair whose full scan found only `noop`/`excluded` comments stores its issue
timestamps under the optional `reconciled` state key; while both timestamps match,
no operation for it is unfinished, and `full_rescan_hours` has not passed, its
comment lists are not read. The stamp uses plan-time timestamps, so activity during
the run triggers a scan next time. The rescan bounds staleness for comment edits that
may not bump issue timestamps.
Transient GETs, including GitHub rate-limit 403s, have bounded retries that honor
`Retry-After` and `x-ratelimit-reset` (capped at 60 seconds). POST/PATCH/PUT requests
do not retry within a run. Apart from item-specific 4xx refusals, a failed request
stops subsequent writes; confirmed earlier results remain durable. Blocked comments
may coexist with independent eligible operations, and produce exit code 2.

Reviewed `blocked` or `unavailable` entries can be acknowledged by operation. State
stores the acknowledged reason (an optional `acknowledged` key, so existing version 2
state stays valid); the entry stays quiet only while that exact reason persists.
Acknowledgement never unblocks a pending intent.

Use least-privilege service credentials and compatible repository/project audiences.
The tool cannot make two APIs atomic. Read/write races, Jira automation, notifications,
concurrent humans, changed permissions, and manual state tampering remain risks.
Preserve the state branch. Restoring old or empty state may discard pending intents
and invalidates the duplicate-safety guarantee.

## Validation

Offline tests cover replay, two-way comments, edits, copy conflicts, deletion rules,
unknown outcomes, pagination, write budgets, privacy exclusions, and durable Git
state with a real local bare repository. Production permissions, Jira required
fields, and branch rules still require a live sandbox pilot.

## Releases and distribution

The reusable workflow is the unit other organizations consume, so it and the binary
are versioned together. release-please writes the release version into the
workflow's `version` default, so the workflow file at tag `vX.Y.Z` runs binary
`vX.Y.Z`; a caller pins one ref and cannot mix a workflow with another release's
tool. A reusable workflow cannot read the ref it was called at, which is why the
version is written into the file rather than derived at run time.

The workflow downloads the release archive for its runner instead of building, so no
Go toolchain or token for this repository is needed; the repository is public, and
the caller's own `GITHUB_TOKEN` reads the release. Before any sync secret is in the
environment, the archive is checked against the release's `checksums.txt` and its
build provenance attestation, which binds it to this repository's
`release-assets.yml` run for the tag. `tool-ref` keeps a build-from-source path for
testing unreleased commits. Release builds are static (`CGO_ENABLED=0`) and
Unix-only, because the state lock uses `flock`.

Releases are immutable once published (GitHub's release immutability), so assets
cannot be attached afterwards; v0.1.0 was published that way and has none.
release-please creates each release as a draft and pushes its tag at once
(`force-tag-creation`, so later runs still find the previous release). The tag push
starts `release-assets.yml`, which waits for the draft, builds and attests the
archives, uploads them, and only then publishes the release. A published release is
never modified; one missing assets is superseded by a new version.

## Jira authentication

Jira is called with HTTP Basic auth (an account email and API token) or, for an
Atlassian service account, with an OAuth 2.0 client credentials grant. An ordinary
user's token works against the site URL. A service account's scoped API token or
OAuth access token works only through the API gateway,
`https://api.atlassian.com/ex/jira/<cloud ID>`, which `jira_cloud_id` selects; OAuth
without it fails validation. Exactly one credential pair must be set, so a leftover
variable cannot silently pick another account.

Only API calls move to the gateway: `jira_url` still
forms the deployment binding and every link written into a copy, so switching an
existing deployment to a service account keeps its state and its links. Because a
wrong cloud ID would write to another site while links point at this one,
`CheckProject` first reads `/rest/api/3/serverInfo` (anonymous, no scopes) and fails
unless its `baseUrl` is `jira_url`.

OAuth access tokens come from `https://auth.atlassian.com/oauth/token` and last an
hour. The client fetches one before the first Jira request and renews it five minutes
before expiry, so a long run never sends a lapsed token. A failure to get a token is
reported as a 401 refusal: the Jira request was never sent, so a creation journaled
just before it becomes `rejected` and is retried next run, instead of staying pending
where recovery could only block it. Token errors name the HTTP status only, never
the response, which could echo the credentials.

Nothing checks which account wrote a copy; Jira's permissions decide. With only Edit
Own Comments, after a switch, edits to originals whose Jira copies the old account
wrote are refused and reported as `rejected`, then retried each run. Granting the new
account Edit All Comments avoids that.

## API references

* https://docs.github.com/en/rest/issues/issues
* https://docs.github.com/en/rest/issues/comments
* https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issues/
* https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-comments/
* https://developer.atlassian.com/cloud/jira/platform/search-and-reconcile/
* https://developer.atlassian.com/cloud/jira/software/rest/api-group-backlog/
* https://developer.atlassian.com/cloud/jira/software/rest/api-group-board/
* https://support.atlassian.com/user-management/docs/manage-api-tokens-for-service-accounts/
* https://support.atlassian.com/user-management/docs/create-oauth-2-0-credential-for-service-accounts/

## Project status

`project_status` generalizes status sync from two states to a configured mapping
between a GitHub Project's single-select field and Jira workflow statuses. Each pair
stores the last agreed option (`project_status` on the pair). Unlike open/closed,
both sides can now change to different values, so that case blocks for review,
alongside disagreement with nothing recorded. An empty project field is not a
change and is filled from Jira, so newly added items converge without review.

The project drives Jira; Jira drives GitHub open/closed. GitHub close/reopen is not
carried to Jira directly, because the project's own workflows already turn it into a
Status change; doing both would move Jira twice, possibly to different statuses.
For the same reason a config uses `status_sync` or `project_status`, never both.
Open/closed entries are planned before project entries, since only open/closed
leaves Jira untouched and so cannot invalidate the other's pre-write checks.

The project is read once per run: the field's options by name over REST, then every
item with that field's value over GraphQL. The REST item listing returns no items to
GitHub App installation tokens (observed live), while GraphQL lists them; option IDs
and item IDs are shared, so single-item reads and all writes stay on REST. A next-page link must stay on the
configured GitHub host. Items are matched to issues by GitHub issue ID. Project calls
use their own client: `GH_PROJECT_TOKEN`, normally a GitHub App installation token
scoped to organization Projects (the reusable workflow mints separate App tokens for
issues and projects, each limited to one repository or owner), and API version `2026-03-10`, under which the
Projects REST endpoints are documented. Adding items and setting the field are
idempotent and not journaled.

Backlog mode (`jira_board` plus one `jira_backlog` option) adds board-backlog
membership to the Jira side, since in team-managed projects it is independent of
status. The Jira side's option becomes "the backlog option if the issue is in the
backlog, else the option covering its status", and everything above applies to it
unchanged, including the agreed baseline. Membership is read once per run from the
board's backlog listing (Agile API `GET /board/{id}/backlog`, every page or the run
fails) and re-read per issue with a JQL filter before and after each write. Writes use
`POST /backlog/{board}/issue` and `POST /board/{board}/issue`; both may answer 207 with
per-issue failures, which the confirming re-read catches. The backlog listing lags a
move by about a second (observed live), so each move polls membership for up to ten
seconds before the confirmation reads it. A board move can itself
change the status (Kanban boards put the issue in the first column), so the
transition planned from the old status is recomputed from the new one when they
differ, and skipped when the new status already maps to the target. Jira omits done
issues from backlog listings, so moving a done issue to the backlog option is
blocked instead of attempted.

## Parent links

`sync_parents` mirrors one level of hierarchy between linked issues: a GitHub
sub-issue's parent and the Jira issue's `parent` (an Epic above a Story, in Jira's
terms). It reuses the agreed-value design of status sync: each pair stores the parent
both sides last agreed on, as a GitHub issue number (0 for none). GitHub parents are
read by listing the children of linked issues whose `sub_issues_summary` is non-zero;
Jira parents come with the issue fields already read. Only linked issues count, so an
unlinked GitHub parent reads as no parent until it is linked, letting creation order
not matter. A Jira parent that is not linked has no GitHub counterpart and blocks.
Writes use GitHub's sub-issue endpoints (`replace_parent` to move, the sub-issue's ID
rather than its number) and Jira's issue edit (`fields.parent` to set,
`update.parent.set.none` to remove). Hierarchy refusals are item-level rejections.

`jira_subtask_type` handles the one case parent sync cannot fix afterwards: an issue
nested under a standard Jira issue must be a sub-task, and Jira fixes both a
sub-task's type and its parent at creation. Creation therefore reads the GitHub
parent (one request per issue to create, only in this mode). When the parent's Jira
issue reports `hierarchyLevel` 0, the issue is created with the sub-task type and
`fields.parent`, and the pair's agreed parent is that parent rather than none. The
decision uses the parent's actual Jira type, not the configured mapping, because the
mapping can change after the parent was created. A selected parent not yet linked
defers the child instead of letting it be created as a standard issue, which would
be irreversible; an unselected parent never will be linked, so it does not. The
parent is re-read before the create request, since it decides an issue type that
cannot change afterwards. CheckProject verifies the ID is a sub-task type.

## Jira → GitHub creation

`jira_selection_label` mirrors GitHub → Jira creation in the other direction. Each run
searches the project for labeled Jira issues (filtered by `issue_state` through Jira's
status category) and plans a GitHub issue for each one not yet linked. The operation
token hashes the Jira issue ID (`jira_issue` kind). The intent is journaled with its
start time before the POST; the created issue's body ends with a hidden marker, so a
lost response is recovered by listing issues updated since the intent started and
matching the marker exactly. No match leaves the intent blocked, as for issue creation
in the other direction; a 4xx refusal marks it rejected and retryable.

The two directions must never feed each other. A GitHub issue that ends with the
tool's marker is never sent to Jira, and a Jira issue carrying the tool's identity
property is never sent to GitHub, even when either carries the other side's
selection label: each is left to its own recovery, or blocked when its journal is
missing. Once linked, a pair created from Jira is indistinguishable from any other.
New pairs record "open" and "no parent" as agreed, so status and parent sync carry
Jira's current values to GitHub on the next run.
