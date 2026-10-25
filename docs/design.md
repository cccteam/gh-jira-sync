# GitHub → Jira issues; GitHub ↔ Jira comments

## Agreed scope

* Jira Cloud, initially Software and business projects.
* Create a Jira issue for each eligible GitHub issue labeled `sync-jira`.
* Copy title and description at creation. Do not subsequently sync issue fields,
  status, labels, assignees, or deletions. Never create GitHub issues from Jira.
* Copy original comments in both directions, including later edits to originals.
* Report edits to copied comments; preserve them for human review.
* Never delete comments or recreate a known deleted copy.

The earlier idea of bidirectional issue-field sync is superseded. Jira-wins field
conflict resolution no longer applies: issue fields are creation-only, and a
comment's original remains its authority.

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
immutable IDs detect transfers/replacements. The created Jira issue has a source
label and issue property set in the same creation request. State records its Jira
ID and key. Recovery searches by label and checks the property; multiple matches
or mismatched identity block adoption.

Before every POST, persist a `pending` operation. In Actions this means successfully
pushing a unique commit to `origin/sync-state`, not just writing runner-local disk.
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

Each copied comment carries an attribution prefix, original link, and stable
operation marker. The journal records original/copy IDs, origin side, envelope,
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

GitHub Markdown is copied literally into Jira ADF paragraphs. Jira text, paragraphs,
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
using a fixed repository-wide concurrency group and disables active cancellation.
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

## API references

* https://docs.github.com/en/rest/issues/issues
* https://docs.github.com/en/rest/issues/comments
* https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issues/
* https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-issue-comments/
* https://developer.atlassian.com/cloud/jira/platform/search-and-reconcile/
