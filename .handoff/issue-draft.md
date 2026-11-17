<!--
DRAFT for an issue in restic/restic. Not posted anywhere.
Title suggestion:
  backup: skip re-reading files of directories completed by an interrupted backup (tracking issue, supersedes #3230)
-->

Output of `restic version`
--------------------------

restic 0.19.1 (the prototype is based on current `master`)

What should restic do differently? Which functionality do you think we should add?
----------------------------------------------------------------------------------

When a `restic backup` is interrupted (Ctrl-C, kill, crash, power loss, network drop), the next run of the same backup should not have to re-read and re-hash the files in directories that the interrupted run had already finished.

Today the next run re-uploads very little, thanks to deduplication and the intermediate index, but it re-reads every file that is new or changed relative to the last *completed* snapshot. For an initial backup, or a large change set, that is the expensive part.

This issue does three things:

1. collects the existing issues, pull requests and forum threads on this topic in one place (lists below);
2. proposes a concrete, small design that revives #3230 and avoids the objections raised against the other approaches;
3. asks one design question before I open a pull request.

A working prototype with tests is here: https://github.com/patrickwolf/restic/tree/backup-resume-journal ([diff against master](https://github.com/restic/restic/compare/master...patrickwolf:restic:backup-resume-journal)).

### Proposal

No new command line options, no change to the repository format, no partial or temporary snapshots.

- **Journal of completed directories, local.** While backing up, each time a directory's tree blob has been saved, restic appends `snapshot path -> tree ID` to a journal file in the repository's cache directory (`<cache>/<repo-id>/resume/<hash of host + target paths>`). Entries are sealed with the repository key, so no paths are stored in plain text. The journal is deleted when the backup completes and discarded after 30 days.
- **Use it as an additional lookup candidate.** On the next run of the same host and targets, when the archiver enters a directory it consults the journal's tree for that path first and the parent snapshot's subtree second. The first candidate whose node matches the file on disk (same check as today: size, mtime, ctime, inode, and all content blobs present in the index) is reused. Internally the archiver takes a short list of previous trees per directory instead of exactly one; with this proposal the list never has more than two entries.
- **The journal is only a hint.** It is the one piece of state that exists only locally, and it holds pointers, not data: the trees themselves are ordinary tree blobs in the repository. A journal entry whose tree is not in the index, or cannot be loaded, is skipped silently. The content of the new snapshot never depends on the journal; if it is missing, stale, truncated, or `prune` removed the trees in between, restic behaves exactly as it does today.
- **Statistics stay relative to the parent snapshot.** Files reused through the journal are still counted as new or changed, as if the interrupted run had not happened.

### Design question

For the journal to be useful, the tree blobs of completed directories have to reach the repository before the interruption. Pack files are currently only uploaded when full, and the tree pack fills slowly, so most completed trees are still in a local temporary pack when restic is killed. #3230 ran into exactly this during testing ("resume found nothing") and solved it by also uploading a pack when it is old enough.

The prototype does the same, restricted as far as I can: only tree packs, only while a journal is in use, and only every 10 minutes, the same interval as the intermediate index. That is at most six small tree packs per hour, which `prune` repacks later.

The earlier upload also helps independently of the journal: `restic recover` builds a snapshot from directories that no snapshot references, so the more trees of an interrupted backup have reached the repository, the more of it `recover` can bring back. #1964 (periodic snapshots during a backup) was closed on the grounds that `recover` "sufficiently addresses this use case"; today that only holds for the trees that happened to be in a full pack.

Is that acceptable? The alternatives I see:

- no early upload: nothing new in the repository, but little benefit until a full tree pack's worth of directories has completed, and none after a hard kill before that;
- keep the tree contents in the local journal instead of pointers: nothing new in the repository at all, but then the cache holds data that exists nowhere else, which @MichaelEischer argued against in the forum ("no longer a cache but rather an additional (temporary) data store");
- store the journal in the repository next to the trees: resuming would then also work from another machine or after losing the cache, but it needs a new kind of file in the repository that has to be deleted again after a successful backup, which is a format change and does not work with append-only repositories;
- upload pending packs and a final index on SIGINT/SIGTERM (suggested in #5289): helps for Ctrl-C, not for kill, crash or power loss. I think this is worth doing independently of this proposal.

What are you trying to do? What problem would this solve?
-------------------------------------------------------

Backing up multi-terabyte sources where a single run takes many hours or days and cannot always run to completion (business hours, laptops, unstable links, power loss). Each interruption throws away the scan progress, and with enough interruptions the backup never completes. #2280 describes the same situation for 5 to 20 TB initial backups.

The saving is the time restic would otherwise spend reading and hashing the files of the directories that were already completed; for those directories the next run only compares metadata, as it does with a parent snapshot. #5289 is an example at this scale (16 TB, about 700,000 files, interrupted several times).

### How this relates to earlier discussion

The approaches discussed so far, and why this proposal differs:

| Approach | Where | Outcome |
|---|---|---|
| Intermediate / periodic / "interrupted" snapshots in the repository | #1183, #1489, #1964, #2960, PR #2961 | Objections: "cluttering the snapshots list", has to be cleaned up again, conflicts with append-only mode ("too breaking IMO"), makes the archiver more complex |
| Search the repository for unreferenced trees and use them as parents | PR #3229 | Withdrawn by the author: as slow as a `check`, and many candidate trees per file |
| Local resume file listing finished directories and their tree IDs | PR #3230 (needs PR #3121) | Tested successfully by a user, never reviewed, closed as stale by the author in 2024; "the general feature is still interesting" (2025) |
| Multiple parent snapshots | #3118, PR #3121 | Not reviewed; wishes were "as little new functionality to users as possible" and a hard limit on the number of parents |

This proposal is #3230 again, with these differences:

- no new options and no change to the snapshot format; it does not depend on user-visible multi-parent support (#3121), and the number of candidates per directory is at most two;
- the journal is encrypted (the resume file in #3230 contained plain text paths);
- it comes with an automated test that interrupts a backup, reopens the repository and checks that only the unfinished directory is read again (#3230 had no test for this);
- journal entries are validated against the index, so a hard kill before an index upload only costs speed.

It does not write anything that looks like a snapshot, so `forget`, `snapshots`, append-only repositories and `--skip-if-unchanged` are unaffected.

### Known limits

- The unit of resume is a completed directory. Files finished inside a directory that was still open at the interruption are read again; one huge flat directory benefits little.
- Running `prune` between the interrupted run and the next one removes the unreferenced trees. The journal entries are then skipped and the run behaves as today.
- Work done in about the last twenty minutes before a hard kill may have to be redone (tree pack upload plus the intermediate index, ten minutes each).
- Two backups of the same host and targets running at the same time share one journal; the first to finish removes it. If the other is then interrupted, it is not resumed.
- Needs the local cache; not used with `--no-cache`, `--force`, `--dry-run` or `--stdin`.

### Related issues and pull requests

Would be resolved by this proposal:

- #2280 Re-use cached information about partial backup to reduce I/O (open)

Motivation largely addressed; the remaining part (restoring data from an unfinished backup) is covered by `recover` since #2056 / #5296:

- #1183 "Incremental" snapshots (open)
- #2960 Save an "interrupted" snapshot when backup is interrupted by SIGINT (open)

Superseded pull requests (closed, not merged):

- #3230 backup: Add resuming from aborted backups
- #3229 backup: Add --resume option
- #2961 Interrupt backup

Same topic, already closed:

- #59 Resume interrupted backups, PR #310 resume interrupted backups (merged 2015; the origin of today's behaviour)
- #385 Handle interrupted backups
- #1211 Interrupted backup not cleaning up nor resuming
- #1489 No intermediate snapshots on large uploads
- #1704 Interrupted backup not resuming
- #1964 Periodically store a snapshot while the backup is still running
- #1966 Data from incomplete snapshot
- #2367 Upload partial index on SIGINT
- #2402 Can unfinished snapshots be restored from?
- #2632 Trust the contents in the index during the creation of the first snapshot
- #4779 Resuming an interrupted backup when using REST backend doesn't work until you manually repair index
- #5287 Let the "recover" cmd fix the "additional files were found in the repo" issue from interrupted backups
- #5288 Option to reject or tag incomplete snapshots
- #5289 Interrupting large uploads leads to re-upload as additional files aren't considered for snapshots
- PR #2975 Add FAQ on whether restic can resume backups (merged)

Related, not resolved by this proposal (multiple parents and parent selection). The internal change here, a list of previous trees per directory, is the building block these need; a follow-up could select several parent snapshots automatically without new options, as suggested in #3118:

- #3118 support for merged backup parents (open)
- PR #3121 backup: Support multiple parents (closed, not merged)
- #2246 Parent-snapshot detection fails with changing --files-from (open)
- #3198 --files-from-raw breaks parent detection and snapshots output (open)
- PR #3405 backup --files-from-verbatim allow merging snapshots (open)
- #4804 Option to create cumulative snapshots (open)
- #22154 backup: allow reusing a parent snapshot when the backup source path has moved (open)

Adjacent, listed for completeness only:

- #4858 Incremental backups without walking the entire tree (open)
- PR #4469 backup: support reading changed files/dirs from a file (open)
- #3792, #4833, #5276 merging or incrementally extending snapshots (closed, not planned)

### Related forum threads

37 threads with about 68,000 views in total; 14 of them (about 28,000 views) ask directly for an interrupted backup to continue without re-reading.

Interrupted backup has to re-read everything:

- [Why can't I resume a partial upload?](https://forum.restic.net/t/why-cant-i-resume-a-partial-upload/314) (2017)
- [What happens if a backup is stopped?](https://forum.restic.net/t/what-happens-if-a-backup-is-stopped/1616) (2019)
- ["Fast path" adding trees after interrupting backup](https://forum.restic.net/t/fast-path-adding-trees-after-interrupting-backup/1697) (2019)
- [Pausing the Initial Backup](https://forum.restic.net/t/pausing-the-initial-backup/2516) (2020)
- [Restic backup behaviour when interrupted - s3 backend](https://forum.restic.net/t/restic-backup-behaviour-when-interrupted-s3-backend/2576) (2020)
- [Interrupting huge backup](https://forum.restic.net/t/interrupting-huge-backup/2815) (2020)
- [Quicker interrupted backup resumption](https://forum.restic.net/t/quicker-interrupted-backup-resumption/3470) (2021; the design discussion that led to #3229 and #3230)
- [After a sudden shutdown backup restarts from scratch](https://forum.restic.net/t/after-a-sudden-shutdown-backup-restarts-from-scratch/3535) (2021)
- [Interrupted backup](https://forum.restic.net/t/interrupted-backup/3843) (2021)
- [First snapshot in looping](https://forum.restic.net/t/first-snapshot-in-looping/3966) (2021)
- [Pause Restic backup? Recover backup from crash?](https://forum.restic.net/t/pause-restic-backup-recover-backup-from-crash/4222) (2021)
- [Snapshot got killed during backup, what happens?](https://forum.restic.net/t/snapshot-got-killed-during-backup-what-happens/5043) (2022)
- [Not finding a parent snapshot, after container running restic exits](https://forum.restic.net/t/not-finding-a-parent-snapshot-after-container-running-restic-exits/5066) (2022)
- [Does restic *actually* resume initial backups?](https://forum.restic.net/t/does-restic-actually-resume-initial-backups/7453) (2024)

Intermediate or partial snapshots:

- [Feature to create incomplete snapshots](https://forum.restic.net/t/feature-to-create-incomplete-snapshots/654) (2018)
- [Checkpoints for large snapshots](https://forum.restic.net/t/checkpoints-for-large-snapshots/1910) (2019)
- [Restore interrupted backup](https://forum.restic.net/t/restore-interrupted-backup/7441) (2024)
- [Efficient backup in serveral tranches](https://forum.restic.net/t/efficient-backup-in-serveral-tranches/8052) (2024)

Parent selection, staged backups of subfolders, multiple parents:

- [Understanding parent directories and subdirs and other understanding questions](https://forum.restic.net/t/understanding-parent-directories-and-subdirs-and-other-understanding-questions/364) (2018)
- [Randomly Needs to Rescan All Data](https://forum.restic.net/t/randomly-needs-to-rescan-all-data/3366) (2020)
- [Change of parent choosing behaviour](https://forum.restic.net/t/change-of-parent-choosing-behaviour/3379) (2020)
- [Restic never finds a parent snapshot](https://forum.restic.net/t/restic-never-finds-a-parent-snapshot/3938) (2021)
- [Add a folder to an existing backup](https://forum.restic.net/t/add-a-folder-to-an-existing-backup/4145) (2021)
- [Restic never finds a parent snapshot when the snapshot exists](https://forum.restic.net/t/restic-never-finds-a-parent-snapshot-when-the-snapshot-exists/4312) (2021)
- [Using relative paths - Restore and parent snapshots](https://forum.restic.net/t/using-relative-paths-restore-and-parent-snapshots/4428) (2021)
- [Backup parent folder doesn't "see" previous backups](https://forum.restic.net/t/backup-parent-folder-doesnt-see-previous-backups/5709) (2022)
- [Backing up multiple paths at once](https://forum.restic.net/t/backing-up-multiple-paths-at-once/6261) (2023)
- [No parent snapshot found, will read all files](https://forum.restic.net/t/no-parent-snapshot-found-will-read-all-files/6947) (2023)
- [Support multiple parents #3121](https://forum.restic.net/t/support-multiple-parents-3121/7220) (2024)
- [New to restic, basic question: should it rescan all files everytime?](https://forum.restic.net/t/new-to-restic-basic-question-should-it-rescan-all-files-everytime/8638) (2024)
- [Un-needed rescanning of files](https://forum.restic.net/t/un-needed-rescanning-of-files/9653) (2025)

What is left in the repository after an interruption:

- [Aborted Backups](https://forum.restic.net/t/aborted-backups/142) (2017)
- [What is the "standard procedure" to follow if a backup or restore is interrupted?](https://forum.restic.net/t/what-is-the-standard-procedure-to-follow-if-a-backup-or-restore-is-interrupted/571) (2018)
- [Packs from interrupted backup reusable after rebuild-index?](https://forum.restic.net/t/packs-from-interrupted-backup-reusable-after-rebuild-index/2666) (2020)
- [Interrupted a backup, remove orphaned data?](https://forum.restic.net/t/interrupted-a-backup-remove-orphaned-data/8629) (2024)
- [Interrupted Backup - Repository Cleanup?](https://forum.restic.net/t/interrupted-backup-repository-cleanup/9446) (2025)
- [Repository corruption after interrupted SFTP backup and non-deterministic repository checks?](https://forum.restic.net/t/repository-corruption-after-interrupted-sftp-backup-and-non-deterministic-repository-checks/10924) (2026)

Did restic help you today? Did it make you happy in any way?
------------------------------------------------------------

TODO
