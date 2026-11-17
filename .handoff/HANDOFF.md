# Handoff: resumable restic backups

Written 2026-11-17 so this work can be continued in a new chat. Everything a
new session needs is in this file and in the two branches named below.

## Goal

After an interrupted `restic backup` (Ctrl-C, kill, crash, power loss), the
next run should not re-read and re-hash files in directories the interrupted
run had already finished. Today dedup avoids re-upload, but every file that is
new or changed relative to the last completed snapshot is read again.

Target: get this accepted upstream in `restic/restic`. Upstream is
conservative; earlier attempts stalled (see "Upstream history").

## Current state

- Upstream issue opened by Patrick: https://github.com/restic/restic/issues/22199
  It asks one design question (early tree-pack upload) before a PR is opened.
- Prototype is implemented, tested and pushed. **No pull request exists yet.**
- Waiting for maintainer feedback on the issue.

## Branches (fork `patrickwolf/restic`)

| Branch | Purpose |
|---|---|
| `backup-resume-journal` | Clean, single commit `1d9c264` on upstream master `55abaa6`. This is the branch linked from the issue and the one to open the PR from. |
| `claude/stoic-albattani-44llgj` | Working branch with step-by-step history (`a041ec2`, `9b3ee8b`, `ba5757d`) plus this `.handoff/` folder. Same code as the clean branch. |

Diff against upstream:
https://github.com/restic/restic/compare/master...patrickwolf:restic:backup-resume-journal

If the code changes, update both: commit on the working branch, then
re-create the single commit on `backup-resume-journal` (it is linked from the
issue, so prefer adding commits over force-pushing once reviewers look at it).
Keep `.handoff/` out of `backup-resume-journal`.

## Design as implemented

1. **Journal of completed directories, local.** Each time a directory's tree
   blob is saved, `snapshot path -> tree ID` is appended to
   `<cache>/<repo-id>/resume/<sha256(host + sorted absolute targets)>`.
   One base64 line per entry, each sealed with the repository master key
   (no plain text paths). Torn or undecodable lines are ignored on load.
   Deleted when the backup completes; discarded when older than 30 days.
2. **Candidate list in the archiver.** `saveTree`/`saveDir` take
   `[]parentTree`, `save` takes `[]parentNode`. On entering a directory the
   candidates are: the journal's tree for that path (if its blob is in the
   index), then the parent snapshot's subtree. For a regular file the first
   candidate with `!fileChanged && allBlobsPresent` wins. At most two
   candidates per directory.
3. **Journal is only a hint.** Journal trees missing from the index or failing
   to load are skipped silently. Missing content blobs behind a journal
   candidate do not raise the "parts of ... not found" error (so no exit
   code 3 on a resumed run). Snapshot content never depends on the journal.
4. **Early tree-pack upload.** Packs are normally uploaded only when full and
   the tree pack fills slowly, so completed trees would usually not be in the
   repo at interruption time. While a journal is active, partially filled
   **tree** packs are flushed every 10 minutes (`treePackFlushInterval`,
   `Repository.FlushTreePacks`). Data packs are not flushed early.
5. **Statistics relative to the real parent.** Files reused via the journal
   count as new/changed, not unmodified (`statsNode`).
6. **No new user options, no repo format change, no partial snapshots.**

Not used with `--no-cache`, `--force`, `--dry-run`, `--stdin`,
`--stdin-from-command`.

### Files touched (vs upstream)

- `internal/archiver/resume_journal.go` (new): `ResumeJournal`,
  `OpenResumeJournal`, `Lookup`, `Add`, `Len`, `Close(remove)`; nil-safe.
- `internal/archiver/resume_journal_test.go` (new): journal round trip, torn
  line, wrong key; `TestArchiverResumeInterrupted`.
- `internal/archiver/archiver.go`: `parentNode`, `parentTree`, `parentFinder`,
  `loadSubtrees`, `loadParentTrees`, `statsNode`, `treeSaved`,
  `Archiver.Journal`.
- `internal/archiver/tree_saver.go`: `completeTree` hook after a tree is saved.
- `internal/repository/repository.go`: `FlushTreePacks`.
- `cmd/restic/cmd_backup.go`: `openResumeJournal`, wiring, "resuming
  interrupted backup, N directories were already completed" message.
- `doc/040_backup.rst`, `changelog/unreleased/issue-2280`.
- `internal/archiver/archiver_test.go`: one call-site adjusted.

## Decisions made, and why

| Decision | Reason |
|---|---|
| Journal holds pointers (path -> tree ID), not tree contents | MichaelEischer (forum thread 3470): data in a cache "which is not available at the data source" is "no longer a cache but rather an additional (temporary) data store". Pointers are disposable; losing them only costs time. |
| Journal stays local, not in the repository | In-repo journal needs a new repo file type that must be deleted after success: a format change, and incompatible with append-only repos. Listed as an alternative in the issue for maintainers to choose. |
| Flush tree packs only, every 10 min | Patrick's concern about many small packs. 10 min matches the intermediate index interval. `prune` repacks small packs once there are >= 10. Also makes more of an interrupted backup recoverable with `restic recover`. |
| Repeatable `--parent` removed from this change | fd0 (#3118): "as little new functionality to users as possible". MichaelEischer wanted a hard limit on parents. Multi-parent is a separate follow-up (revive #3121); the internal candidate list is its building block. |
| Encrypt journal entries with the repo key | Cache otherwise contains only encrypted data; #3230's resume file was plain text. |
| Journal key = host + targets (not tags/excludes) | Journal is a lookup source only, so changed excludes are harmless. |
| Resumed run keeps appending to the same journal | Several interrupted runs in a row accumulate until one succeeds. |
| No timing numbers in the issue | Too much work to measure; benefit follows from the mechanism. #5289 cited as an example at scale. |

## Known limits (all stated in the issue)

- Unit of resume is a completed directory; a huge flat directory gains little.
- `prune` between runs removes the unreferenced trees; run then behaves as today.
- Up to about 20 minutes of work before a hard kill may be redone (tree-pack
  flush + index age, 10 min each).
- Two concurrent backups of the same host + targets share one journal; the
  first to finish deletes it.

## Upstream history (researched; quotes verified against fetched text)

- **PR #3230** (aawsome, 2021) is this design: local resume file with tree
  IDs + extra parent trees, depended on PR #3121. A tester first got "resume
  found nothing" because trees were not yet uploaded; the author added
  age-based pack upload. Never reviewed; author closed it as stale 2024-02.
  MichaelEischer 2025-09: "the general feature is still interesting.
  Although I haven't even had time to look at the concept of the feature."
  It had no automated test and a plain text resume file.
- **PR #3229**: scan repo for unreferenced trees. Withdrawn: as slow as `check`.
- **#2960, #1964, #1489, #1183, PR #2961**: intermediate/interrupted snapshots
  in the repo. Objections: "cluttering the snapshots list", cleanup, breaks
  append-only ("too breaking IMO", rawtaz), archiver complexity.
- **#2280**: the open issue for exactly this problem.
- **#3118 / PR #3121**: multiple parents; not reviewed.
- **MichaelEischer's stated preference** (#5289, 2025-03): upload a final
  index on SIGINT/SIGTERM. Complementary; candidate for a separate small PR.
- rawtaz repeatedly asked "is it worth it / how much quicker" on earlier
  attempts. Expect the question; one rough number from a real run answers it.
- What killed earlier attempts was mostly **review capacity**, not rejection.
  Keep the PR small.

The full catalogue (39 GitHub items, 37 forum threads) is in the issue body;
a copy of the last draft is `.handoff/issue-draft.md` (the posted issue may
differ slightly from it).

## Test status

Passing: `go test -race ./internal/archiver/` (new tests included),
`go test ./internal/repository/`, `go test ./cmd/restic -run Backup`.

Not done:
- **Real kill-and-resume of the binary** on large data. Needs a backup running
  longer than 10 minutes; this is also where a timing number would come from.
- **`restic recover` after an interrupted run** to confirm more is recovered.
- Full `cmd/restic` test suite; Windows/macOS runs (only `go vet` with
  `GOOS=windows`).
- `TestArchiverErrorReporting`, `TestScannerError`, `TestBackupErrors` fail
  when run as root (they rely on unreadable files); the first was confirmed to
  fail without these changes too. Skip them or run as non-root.

## Next steps

1. Watch issue #22199 for maintainer answers, especially on the early
   tree-pack upload. Alternatives offered there: no early upload; tree
   contents in the local journal; journal in the repository; flush on
   SIGINT/SIGTERM only.
2. Adjust the prototype to the answer, then open the PR from
   `backup-resume-journal` against `restic/restic` (use the repo's PR
   template; the changelog entry already exists as `issue-2280`, rename if the
   maintainers prefer the new issue number).
3. Optional separate PRs: final index (and pack) upload on SIGINT/SIGTERM;
   automatic multi-parent selection (#3118).
4. Optional extension, not built: per-file journal entries for huge flat
   directories.

## Practical notes for a new session

- Commits on these branches carry a `Co-Authored-By` trailer and a session
  link. Patrick has not said whether to keep them for the upstream PR; ask.
- Research was done through the webclaw scraping API (`/v1/scrape`,
  `/v1/batch`, `/v1/search`) because forum.restic.net and api.github.com were
  not reachable directly. The API token is deliberately **not** in this file;
  ask Patrick for it if more research is needed. A cloud environment named
  "restic" that allows `api.webclaw.io` was created for this.
- `restic/restic` itself is not attached to the session; only
  `patrickwolf/restic` is. Reading upstream issues needs the scraper or web
  fetch; posting upstream is done by Patrick.
- Patrick prefers research to be delegated to cheap sub-agents, focused on
  essential sources, with interim check-ins before crawling further.
