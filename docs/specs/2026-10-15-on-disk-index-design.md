# On-disk index snapshots: design

Status: draft for review. Builds on the [in-memory B+ tree](2026-10-12-in-memory-btree-design.md), the [engine index wiring](2026-10-12-engine-index-wiring-design.md), [CREATE INDEX](2026-10-15-create-index-design.md) (PR #43 and part B, PR #44) and the [page file and cache](2026-10-06-page-file-and-cache-design.md).

## Goal

Make a restart cheap. Today every table's trees are rebuilt from one full scan of its heap the first time a statement needs them, so the first statement on a large table after a restart reads and decodes every row. With this change a clean shutdown saves the trees to a file next to the heap, and the next start loads them without reading the heap.

## Context

The index is a rebuildable, in-memory cache of the heap. The heap write is the commit point, there is no WAL, and a tree is discarded whenever the file might disagree with it. Nothing about an index is durable today except the CREATE INDEX definitions in `sys_indexes`.

The earlier specs anticipated an on-disk node format ("fixed fan-out nodes", "page ids instead of pointers"). This spec does not do that. It saves a snapshot, because the goal is restart time, not memory: once loaded, the trees are the same in-memory trees as now. Trees larger than memory, and a tree read page by page through the page cache, remain out of scope.

What makes this hard is not the file format but trust. A saved tree may be used only if the heap has not changed since it was saved, and with no WAL nothing records a heap change except the heap itself. The design therefore centres on a protocol that makes a stale snapshot impossible to load, even after a crash.

## Decisions made during design

- **A snapshot, not paged nodes.** The file holds each tree's sorted entries, not its nodes. Loading inserts the entries in order into a fresh in-memory tree: no heap scan, no row decoding, no dependency on the tree's node layout, so the tree can change without changing the format.
- **One file per table**, `<data_dir>/data/<db>/<file_id>.idx`, holding every tree of that table (the PRIMARY KEY/UNIQUE columns and the CREATE INDEX definitions). Named by the heap's file id like the heap, so renaming a table never touches it.
- **Saved at clean shutdown only.** A crash leaves no snapshot, which means a rebuild, exactly as today.
- **Invalidated before the first heap write.** The snapshot file is removed, and the removal made durable, before the first statement that changes the heap after the table is opened. A crash at any point therefore leaves either a snapshot that matches the heap or no snapshot.
- **Loaded lazily**, the first time a statement needs the trees, in place of the scan that builds them today. Any doubt about the file falls back to that scan.
- **A snapshot is an optimisation, never state.** Every failure to save or load is invisible except in speed. The heap stays authoritative.

## Design

### File format

`<file_id>.idx` is a `storage.PageFile`: 16KiB pages, each with the checksum and page number `PageFile` already verifies. The owner's layout, after the 8-byte page header:

| Page | Content |
| --- | --- |
| 0 | Header: magic, format version, the heap's `NumPages()` at save time, the number of trees, then one descriptor per tree |
| 1.. | Entry pages, the trees one after another in descriptor order |

A tree descriptor holds:

- its identity: kind (unique column or CREATE INDEX), the column's schema index and name for a unique column, or the index name and column name for a definition;
- whether it is degraded (a `nil` tree: a column or index that cannot hold the file's values), in which case it has no entries;
- its entry count and the page number of its first entry page.

An entry page holds a run of consecutive entries of one tree: a 2-byte count, then each entry as a 2-byte key length, the key, the page number (4 bytes) and the slot (2 bytes), the same sizes the in-memory tree already accounts for. Keys are the codec's bytes, unchanged. Descriptors that do not fit page 0 are not expected (a table has few keys and indexes); a table whose descriptors would not fit simply does not save, and rebuilds.

The page checksum catches a torn or damaged page. The header catches a snapshot for a different heap or definition set.

### When a snapshot is trusted

A table loads its snapshot instead of scanning only if all of these hold, checked in this order; any failure removes the file (it can never become valid again) and takes the scan path:

1. The file opens as a page file and every page read verifies.
2. Magic and format version are known.
3. The heap page count in the header equals the heap's `NumPages()` now.
4. The set of trees in the header equals, exactly, the set the table has now: the same PRIMARY KEY/UNIQUE columns (schema index and name) and the same CREATE INDEX definitions (name and column).
5. For every tree, the entries are strictly increasing in key order and their number equals the descriptor's count.

The page count is a cheap sanity check against a file copied from somewhere else; it is not what makes the snapshot sound. That is the next section.

### Making a stale snapshot impossible

A snapshot is sound if no heap write happened between the save and the load. Two things can break that: a write that never built the trees, and a crash.

- **On the first mutation of the heap after open, the snapshot file is unlinked and its directory fsynced, then the write proceeds.** "Mutation" is exactly the three places that change an existing heap file: `appendToFile` (INSERT), `deleteFromFile` (DELETE) and `Update`. ADD/DROP COLUMN writes a new heap file under a new id, so the old file's snapshot is an orphan and the new heap has none. The table remembers that it has done this, so the cost is once per open, not per statement.
- If the unlink or the directory fsync fails, the statement fails before writing anything. Writing the heap with a snapshot that might survive a crash would be the one way to corrupt an index silently.
- A clean `Close` writes a new snapshot to `<file_id>.idx.tmp`, fsyncs it, renames it over `<file_id>.idx`, and fsyncs the directory. The file is therefore absent or complete. It saves only if the trees are built (otherwise there is nothing newer than what is on disk, and the file on disk is still valid unless a mutation removed it).
- Crash after a heap write and before the next `Close`: no snapshot exists (it was removed before the write). Crash during the save: the final name is either the previous snapshot or absent; only a `.tmp` is left, which is removed by the next save or by orphan cleanup.

What this does not defend against is something other than this process changing a heap file between runs. That is not a threat model the heap has either.

### Lifecycle

- **Open / first use.** `indexSet()` tries to load before it builds. On success the loaded set is installed exactly as a built one is.
- **Close.** `Table.Close` saves, then closes the heap. A save failure is ignored: the file is a cache, the next start rebuilds, and failing a shutdown over it helps nobody. `Catalog.Close` already closes every table.
- **CREATE INDEX / DROP INDEX.** The trees are already discarded; the snapshot is removed too, since its tree set no longer matches. (Check 4 would reject it anyway; removing it avoids leaving a file known to be useless.)
- **DROP TABLE** removes the `.idx` and `.idx.tmp` with the heap. **RENAME TABLE** needs nothing. **RENAME COLUMN** needs nothing: a snapshot saved before the rename fails check 4 and is rebuilt; one saved after it has the new names.
- **ADD / DROP COLUMN** orphan the old snapshot as described above.
- **Startup cleanup.** `removeOrphans` and `maxTableFileID` learn two more names, `<number>.idx` and `<number>.idx.tmp`, with the existing rules: decided by id, never recursively, only files named by a number and that suffix. An `.idx` whose id no table references is removed with its heap. A directory counts as empty only when these are gone too.

## Error handling

- Failing to read a snapshot is never an error to the user: remove the file, scan.
- A failure to save is swallowed.
- A failure to remove a snapshot before a heap write is returned to the user, and nothing is written.
- Snapshot code never returns a wrong tree: a corrupt page, a mismatched header or an out-of-order entry all end in the scan path.

## Out of scope (YAGNI)

- Trees paged through the page cache, and indexes larger than memory.
- Saving periodically or on checkpoints, so that a crash also gets a fast restart. The protocol above is what such a thing would need to be built on; it would also need a way to know which writes the snapshot already includes.
- Incremental update of a snapshot, compression, and a configuration switch to turn snapshots off.
- Copying a data directory between machines with snapshots in it. (The page-count check makes the common mistakes fail safe; nothing more is promised.)

## Testing and verification

- **Format.** Save and load round trip for a table with unique columns, definitions, duplicates in a secondary tree, a degraded tree, an empty tree and no trees; the loaded set equals a fresh build (`assertIndexesMatchHeap`). Entries spanning several pages. A table with no trees writes nothing.
- **Trust checks**, each with its own test that falls back to the scan and removes the file: damaged page (flip a byte), unknown version, wrong page count, different tree set (an index added, an index dropped, a column renamed), entries out of order, wrong count.
- **Protocol.** A restart test that proves the loaded trees avoid the scan: reopen, run an indexed statement, and assert zero `Scan` calls on the counting wrapper. A heap write removes the snapshot before it happens (a failing wrapper on the unlink makes the statement fail and leave the heap untouched). A crash after a write (restart without `Close`) rebuilds and still answers correctly. A crash between the temp file and the rename leaves the old state.
- **Cleanup.** Orphan `.idx` and `.idx.tmp` files are removed at startup, a referenced one is not, and nothing outside `<number>.idx` is touched.
- **Differential.** The existing seeded unindexed-vs-indexed comparison extended with random restarts (clean and crashed), so answers, errors and files stay equal across them.
- **Mutation checks**, per the project's usual practice, including: skipping the unlink before a write, loading without checking the page count, loading without checking the tree set, skipping the sortedness check, saving when the trees are not built, and removing orphans by the wrong pattern.
- One-off measurement quoted in the PR: first indexed statement after a restart on a 100,000-row table, with and without a snapshot.

## Build order and PR split

1. **Snapshot format** (`internal/index`): serialise a set of trees to a `PageFile` and read it back, with the checks of the trust section. Pure, no engine dependency.
2. **Engine wiring**: load in `indexSet`, save in `Close`, the unlink-before-write protocol, DDL removal, orphan cleanup, restart tests and the differential extension.

Two PRs, because the first can be reviewed and tested without any of the engine's concurrency or lifecycle questions.

## Open questions

- Whether the unlink should happen at the first mutation or, more cheaply, at first load plus at first mutation of a table whose snapshot was never loaded. The spec takes the simpler rule (at the first mutation, whatever happened before); the plan should confirm there is no measurable cost for write-heavy workloads.
- Whether `Close` should log a failed save. There is no logger in the engine today, so the spec swallows it.
