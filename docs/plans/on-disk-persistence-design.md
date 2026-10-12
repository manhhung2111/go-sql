# On-disk persistence for the engine: design spec

Date: 2026-09-30
Status: draft, awaiting review

## 1. Goal

Make the engine durable and disk-resident:

- Databases, table schemas and row data survive a server restart.
- `SELECT` scans table files page by page and keeps only matching, projected rows in RAM. Row data is never held in RAM as a whole table.
- Every mutating statement writes through to disk and fsyncs before it returns.

Non-goals: WAL or crash-atomic multi-page writes, buffer pool, indexes (a later B-tree replaces the uniqueness scan), transactions, TOAST/overflow pages, space reclamation of tombstoned rows, multi-process access.

## 2. Decisions (agreed during brainstorming)

| Topic | Decision |
|---|---|
| Row storage | Disk-resident heap files of 16KiB slotted pages |
| DELETE / UPDATE | Tombstones in place (slot `Offset=0, Length=0`); UPDATE = insert new version, then tombstone old |
| Durability | Write-through plus one fsync per statement; no buffer pool |
| Catalog | System tables in the same heap format (`sys_databases`, `sys_tables`), hard-coded bootstrap schemas, loaded into RAM at startup |
| Schema serialization | JSON with tagged variants, external type-switch functions |
| File naming | Table files are named by numeric `file_id`, never by table name |
| ALTER TABLE ADD/DROP COLUMN | Rewrite into a new file, then swap; folded into sub-project 2 |
| PK / UNIQUE checks | Stream the file against a small set of the statement's own key values (memory O(statement), time O(table)); an index replaces the scan later |

## 3. Decomposition

Four PRs, each with its own plan, in order. Every PR must leave `main` green and working, because CI requires it.

1. **Storage** (`internal/storage`): page decode, positional-I/O `File`. Merged as PR #4.
2. **PR A, plumbing** (`internal/engine`, config, wiring): the row codec (5.1) and table files (5.2): `data_dir` config, id-named files, each `SqlTable` creating its file on `CREATE TABLE` and dropping it on `DROP TABLE` / `DROP DATABASE`. Statements keep running against in-memory `Rows`; the file exists but is empty and unused until PR B.
3. **PR B1 to B6, the switch to disk** (`internal/engine`, 5.3 to 5.5), one PR per statement, merged in this order (see 5.0 for why the order matters):
    - **B1 `INSERT`** appends every new row to the file as well as `Rows`.
    - **B2 `ALTER TABLE`** `ADD`/`DROP COLUMN` rewrite the file into a new file (new `file_id`) in the new schema.
    - **B3 `DELETE`** tombstones the matching rows in the file as well as removing them from `Rows`.
    - **B4 `UPDATE`** inserts the new versions and tombstones the old ones in the file as well as updating `Rows`.
    - **B5 `SELECT`** reads from the file instead of `Rows`.
    - **B6 cleanup** moves the `PRIMARY KEY`/`UNIQUE` checks onto a file scan, makes `ALTER` rewrite from the file, deletes `Rows`, and moves the tests that read `table.Rows` onto `Select`.
4. **PR C, catalog persistence** (`internal/engine`): system tables, schema codec, startup load, crash recovery. Split into three stacked PRs C1 to C3 (see section 6).

Only B1's detailed plan is written up front. Each later plan is written after the previous PR merges, because it cites the helper names and code that PR actually produced.

Until PR C lands, schemas are not persisted: after a restart the server forgets its tables and their files are orphaned on disk. File ids are derived from a scan of the data directory at startup, so a new table never collides with a leftover file, and creating a file never overwrites an existing one.

## 4. Sub-project 1: storage layer

### 4.1 Page

The page wraps its raw 16KiB buffer directly. The old split between a forward-growing `Data` accumulator and backward-growing slot offsets goes away. The on-disk byte layout is unchanged (header 12 bytes little-endian: checksum 0:4, page number 4:8, slot count 8:10, data start 10:12; slot array after the header; row data growing back from the page end; CRC32-IEEE of `data[4:]`).

```go
type Page interface {
    InsertRow(rowBytes []byte) (slot int, err error)
    Row(slot int) ([]byte, bool)   // false: tombstone or out of range
    DeleteRow(slot int) error      // slot Offset=0, Length=0
    SlotCount() int                // includes tombstoned slots
    PageNumber() int32             // position in the file; File checks it on read
    Encode() []byte                // stamps the checksum, returns the page's own buffer (not a copy)
}
func NewPage(pageNumber int32) Page
func DecodePage(data []byte) (Page, error)
```

- Header field `RowCount` is renamed `SlotCount`: it counts every slot ever allocated. Tombstoned slots stay in the array so `RowID`s never shift.
- Space rule is unchanged: inserting needs `DataStartPos - pageHeaderSize - (SlotCount+1)*slotSize >= len(row)`. No slot or space reuse yet.
- A live row never has `Offset == 0` (the header occupies the first 12 bytes), so `Offset == 0` is an unambiguous tombstone.
- `DecodePage` requires exactly `maxPageSize` bytes, a matching CRC32, `DataStartPos` within bounds, and every live slot inside the row area and not overlapping the slot array. Otherwise it returns an error.
- `Row` returns a sub-slice of the page buffer; callers must copy if they keep it.

### 4.2 File

```go
type RowID struct { Page int32; Slot int }

type File interface {
    Insert(rowBytes []byte) (RowID, error)
    Delete(id RowID) error
    Scan() iter.Seq2[Row, error]   // Row{ID RowID; Bytes []byte}, live slots only
    Sync() error
    Close() error
}
func CreateFile(path string) (File, error) // fails if the file exists
func OpenFile(path string) (File, error)   // fails if it does not exist
```

- Opened `O_RDWR` (plus `O_CREATE|O_EXCL` for `CreateFile`), no `O_APPEND`. All I/O is `ReadAt`/`WriteAt` at `pageNumber*maxPageSize`.
- On open, `numPages = size / maxPageSize`. A size that is not a multiple of `maxPageSize` is a "torn or corrupt file" error.
- The tail page is cached in memory. Every write goes straight through to disk, so the cache always equals what is on disk. `Insert` targets the tail page and appends a new page when the row does not fit.
- `Insert` and `Delete` write the affected page but do not fsync; the caller calls `Sync()` once at the end of a statement.
- `Scan` reads page by page, verifies each checksum and yields live rows. The yielded bytes are valid only until the next iteration. A checksum error is yielded as the iterator's error.
- Reading a page also checks that its header's page number equals its position in the file, so a valid page copied to the wrong place is reported (`page N: header says page M`).
- If a write fails after the tail cache was modified, the cache is dropped and reloaded from disk on the next operation, so a change that never reached disk is not flushed by a later write.
- `Close` closes the file; it flushes nothing because there is no buffered state.

Status: implemented and merged (PR #4). API changes versus PR #3: `NewFile` is replaced by `CreateFile`/`OpenFile`, `InsertRow` is now `Insert` returning a `RowID`, `Close` no longer flushes, `Encode` returns the live buffer.
- `File` is not goroutine-safe; the owning `SqlTable` mutex serializes access. Documented on the interface.
- `maxRowSize` guard is unchanged.

### 4.3 Failure model

A torn 16KiB page write is detected by the checksum (`page N: checksum mismatch`) but not repaired. Repair needs a WAL or double-write buffer, which is out of scope.

### 4.4 Tests (testify, `t.TempDir()`)

- Page: encode/decode round trip; a flipped byte fails the checksum; bad slot bounds rejected; tombstone semantics; exact-fit space cases.
- File: insert then scan; reopen preserves rows and continues `RowID` numbering; delete leaves other `RowID`s stable and scan skips the tombstone; insert after delete does not reuse a slot; rollover across pages; partial-size file fails to open; a corrupt page surfaces an error mid-scan; `CreateFile` on an existing path fails.
- The existing "Close flushes the last page" tests are replaced.

## 5. Tables on disk (PR A = 5.1 + 5.2, PRs B1 to B6 = 5.3 to 5.5, one per statement)

### 5.0 Transition: the file is a shadow copy until B5

Status: complete. B1 to B6 are done; `Rows` no longer exists, so the shadow-copy rules below describe the transition that has finished.

A table cannot have some statements on disk and others in RAM, so B1 to B4 keep `Rows` as the authority and make the file a **shadow copy**: every mutating statement that has been migrated also applies itself to the file, and nothing reads the file until B5 flips `SELECT`.

Rules that keep every intermediate `main` correct:

- **Order.** `INSERT` first (it creates the rows), `ALTER` second, then `DELETE`, `UPDATE`, and `SELECT` last. `ALTER` goes before `DELETE`/`UPDATE` because it changes the schema the file's rows are decoded with: once any statement scans the file, a file holding rows in an old column layout would make that statement fail. After B2 the file is always in the table's current schema. `ALTER`'s rewrite builds the new file from `Rows`, which also re-syncs a file that earlier un-migrated statements had left behind.
- **Benign drift.** Between B1 and B5 a not-yet-migrated statement (`DELETE` before B3, `UPDATE` before B4) changes `Rows` but not the file, so the file can drift from `Rows`. That is harmless while nothing reads the file, and a migrated statement's file step never fails because of it: tombstoning a row that is not there matches nothing. This relies on `UPDATE` never assigning NULL (the parser only accepts a STRING or NUMBER literal after `SET`): if that ever changes, re-check that a migrated statement's file step cannot fail where its `Rows` step succeeded.
- **Mirror tests.** Each PR proves its statement keeps the file equal to `Rows`: the test scans the file, decodes it with the table's schema, and compares it to `Rows` (as a multiset for `UPDATE`, which moves updated rows to the end of the file). B5 can flip `SELECT` safely because by then all four mutators are mirrored. From B5 on, `SELECT` reads the file, so a partial I/O failure that left duplicate or missing row versions in the file (spec 5.4 accepts this without a WAL) becomes visible to queries until an `ALTER` rebuilds the file from `Rows`.
- **Uniqueness stays on `Rows`** until B6; only then does it stream the file (see 5.3).
- **PR C must stay after B6.** The drift argument above only holds because nothing on disk survives a restart until PR C persists the catalog. If reopened files could be read by a build whose `SELECT` already reads the file, drift left by an older build would show up in query results.
- **Accepted behaviour changes**, visible to clients from the PR that introduces them: a row whose encoding exceeds one page (`storage.MaxRowSize`, 16368 bytes) is rejected with `row too large` from B1 on, because there are no overflow pages (spec non-goal); from B2 on, `ALTER ... ADD COLUMN` can fail with `row too large` when a row would no longer fit on a page after the new column, and `ALTER ... ADD/DROP COLUMN` on a closed table fails with `table "x" is closed` (both all-or-nothing); from B3 on, `DELETE`, from B4 on `UPDATE`, and from B5 on `SELECT`, on a closed table fail with the same `closed` error, and from B4 on an `UPDATE` that makes a row too large for a page fails with `row too large` (all-or-nothing); and after B4/B5 the order of `SELECT` rows after an `UPDATE` is unspecified, because updated rows move to the end of the file.

### 5.1 Row codec (`internal/engine/rowcodec.go`)

```go
func EncodeRow(columns []parser.ColumnDefinition, row []any) ([]byte, error)
func DecodeRow(columns []parser.ColumnDefinition, b []byte) ([]any, error)
```

- Layout: per column an `(offset uint16, length uint16)` entry, then the payloads. The column count is not stored; the schema is passed in.
- NULL is offset `0xFFFF`. An empty string is a real offset with length 0, so `""` and NULL are distinct.
- Values: int64 as 8 bytes little-endian, bool as 1 byte, strings as raw UTF-8. The codec covers exactly the Go types `coerceValue` can return (to be confirmed against `coerceValue` during planning).
- `DecodeRow` validates every offset and length against the buffer and returns an error, never a panic.

### 5.2 SqlTable

- `Rows [][]any` is removed. `SqlTable` keeps `Name`, `Columns`, `mu`, `fileID` and a `storage.File`.
- `evalWhere`, `coerceValue`, uniqueness sets and projection are unchanged because rows decode to `[]any`.
- Layout on disk: `<data_dir>/data/<db>/<file_id>.tbl`. The next `file_id` is `1 + max id found under <data_dir>/data/`, computed once at startup, then an in-process counter. Ids are never reused.
- `Table` gains `Close() error` (idempotent) and `Drop() error` (close and remove the file). `Table.Rename(name)` stays schema-only and returns nothing; no file operation happens on rename.
- A `fileAllocator` shared by the whole catalog hands out paths. At startup it scans `<data_dir>/data/*/*.tbl` for the highest id, then an atomic counter continues from there.
- `NewCatalog(dataDir DataDir) (Catalog, error)` (config `storage.data_dir`, default `data`); engine tests use `t.TempDir()`. `Catalog`, `Database` and `Table` all gain `Close()` so tests and shutdown can release file handles.
- A `Database` owns its directory: `NewDatabase` validates the name (no path separators, not `.` or `..`) and creates `<data_dir>/data/<db>/`; `Database.Drop()` drops every table, then removes the directory with a non-recursive `os.Remove`. It never deletes a file it did not create, because two database names that differ only by case or Unicode form can map to one directory on a case-insensitive filesystem.
- Dropping follows the "commit first, remove files second" rule: the table or database leaves the in-memory map first, and a failure removing its files is returned as an error that says so. The result is an orphan file, never a catalog entry pointing at nothing.

### 5.3 Statement flows

- **INSERT**: phases 1 and 2 (resolve columns, fillers, coercion, constraint checks) unchanged. Uniqueness is checked by streaming the file, never by loading it: build a small set from only the key values this statement inserts (a duplicate inside the batch is caught while building it), then scan the file once, a page at a time, and fail on any existing row whose key is in that set. Memory is proportional to the rows in the statement, not the table; time is still one full scan per statement until an index exists. If the table has no PRIMARY KEY or UNIQUE column the scan is skipped. All rows are encoded before any write so encode and size errors surface first. Then `Insert` per row and one `Sync()`.
- **SELECT**: same logic with `Scan` as the row source. Only matching, projected rows are kept. A scan error is returned.
- **DELETE**: collect matching `RowID`s first (an `evalWhere` error touches nothing), then tombstone them all, then `Sync()`.
- **UPDATE**: scan to find matches and hold the matched rows in RAM; check uniqueness the same streaming way (the values being assigned against every row that is not being updated); build all updated rows with all checks; then insert new versions, tombstone old ones, `Sync()`. Collecting before applying prevents re-scanning our own appended rows.
- **ALTER ADD/DROP COLUMN**: scan the old file, transform each row, insert into a new file with a new `file_id`, `Sync()`, swap the table's file handle and schema, delete the old file. **RENAME COLUMN** stays schema-only (rows are positional).
- **DROP TABLE**: remove the map entry and drop the file.
- **RENAME TABLE**: unchanged in-memory re-keying; the file is not touched.

### 5.4 Guarantees

Validation errors stay all-or-nothing, as today. An I/O failure or crash partway through applying a statement can leave it partially applied (no WAL). UPDATE inserts before tombstoning so a crash can duplicate a row but never lose one.

### 5.5 Tests

- Codec: round trip across every type, NULLs, `""` vs NULL, truncated and out-of-range offsets return errors.
- Table: the existing INSERT/SELECT/DELETE/UPDATE/ALTER matrices run against a `t.TempDir()` table through a `newTestTable` helper.
- Reopening a file and scanning yields the same rows; drop then recreate starts empty; rename changes no file; ALTER swaps files; the `-race` concurrency tests remain.

## 6. Sub-project 3: catalog persistence

Delivered as three stacked PRs (see `plan/pr-c-catalog-persistence.md`): **C1** foundations with no behaviour change (schema codec, file ids, durable directories), **C2** a catalog store that DDL writes to but nothing reads yet, **C3** startup load, crash recovery and `Close` on shutdown. C3 is the only PR that changes what survives a restart.

### 6.1 System tables

Hard-coded schemas in Go (the bootstrap), stored as heap files under `<data_dir>/sys/`:

- `sys_databases(name TEXT)` in `sys_databases.tbl`
- `sys_tables(db TEXT, name TEXT, file_id BIGINT, schema TEXT)` in `sys_tables.tbl`

The catalog reads and writes them through the row codec and `storage.File` directly, not through the SQL layer. A row is bounded by `storage.MaxRowSize`, so a schema whose JSON does not fit makes `CREATE TABLE` fail with a clear error before anything is created. That is a stated limitation (no overflow pages), not a bug.

### 6.2 Schema codec (`internal/engine/schemacodec.go`)

JSON of `[]ColumnDefinition`, encoded by external type-switch functions:

- DataType tags: `char`, `varchar`, `text`, `smallint`, `mediumint`, `int` (each with `size`), `bigint`, `boolean`.
- Constraint tags: `not_null`, `unique`, `primary_key`, `default`. A default carries a token tag and its literal text. The parser only accepts `STRING` and `NUMBER` defaults, so the tags are `"string"` and `"number"`; the `TokenType` integer is never stored, because it is an `iota` and any new token would shift it. `Position` is dropped.
- An unknown tag on decode is an error, and an unknown Go type on encode is an error.

### 6.3 Startup (`NewCatalog(dataDir) (Catalog, error)`)

1. Create `sys/` and `data/` durably; open or create the two system files.
2. Create the file allocator by scanning `data/*/*.tbl` for the highest id. This happens before any cleanup, so an id that is about to be deleted is still never reused.
3. Load `sys_databases` in append order. A repeated name keeps the first row; later copies are tombstoned.
4. Scan `sys_tables` in append order, keeping a set of *survivors*. A row whose database is not loaded is tombstoned. Otherwise, for the new row `r`, any current survivor with the same `(db, name)` or the same `file_id` as `r` is displaced (its row tombstoned and removed from the survivors), then `r` becomes a survivor. **Step 4 only tombstones rows; it never deletes a file.** Checking against current survivors, not against every earlier row, matters: the rows `[a,f1]`, `[b,f1]`, `[a,f2]` (a rename whose old row was never tombstoned, then an ALTER) leave `{b,f1}` and `{a,f2}`, and `f1` must stay because `b` uses it.
5. Decode every survivor's schema and open `data/<db>/<file_id>.tbl`. A missing file, a missing or torn table file, or any damaged page in a system file, is a startup error (a damaged page inside a table file shows up when that table is first scanned): the server refuses to start rather than guess. On any failure, every file already opened is closed.
6. Orphan cleanup, after every survivor is open. A `*.tbl` file whose numeric id is not the `file_id` of any survivor is removed, in any directory (ids are global, so the id decides, never the directory name). A directory under `data/` that is empty and is not the name of a loaded database is removed with a non-recursive `os.Remove`. Nothing else is ever removed: no `RemoveAll`, no file whose name is not `<number>.tbl`. Removal errors are ignored; an orphan is harmless. There are no `.tmp` files in this design, so nothing cleans them.

Only metadata is kept in RAM. `SHOW DATABASES` never touches disk.

**Case-insensitive filesystems.** Databases `Shop` and `shop` are distinct catalog entries but share a directory on macOS's default filesystem. Because cleanup decides by `file_id` and only removes empty directories, a live database's files are never removed because of its twin; the worst case of removing an empty directory is that the next `CREATE TABLE` recreates it.

### 6.4 DDL ordering (the catalog row is the commit point)

The in-memory state changes only after the catalog write succeeds. A failed catalog write changes nothing in memory and discards any new file it had created.

- **CREATE DATABASE**: create the directory durably, then insert the `sys_databases` row and `Sync`, then add to the map. First tombstone any stale `sys_tables` rows left under this name by an earlier failed drop (below), so a dropped database's tables are never resurrected by a new one with the same name.
- **CREATE TABLE**: validate, allocate the id, **encode the `sys_tables` row first** (so an oversize schema fails before any file exists), create the file durably, insert the row, `Sync`, add to the map.
- **DROP TABLE**: tombstone the row and `Sync` (the commit point), remove from the map, remove the file.
- **DROP DATABASE**: tombstone the `sys_databases` row and `Sync` (the commit point), remove from the map, tombstone that database's `sys_tables` rows and `Sync`, remove the files and directory. If tombstoning the table rows fails, the stale rows stay in the store's index and are swept by the next `CREATE DATABASE` of that name, and by startup (their database row is gone).
- **Replacing a row** (RENAME TABLE, ALTER ADD/DROP/RENAME COLUMN): insert the new row, `Sync`, then tombstone the old row, `Sync`. Two syncs, not one: with a single sync a power loss can persist the tombstone's page without the insert's page and the table would vanish from the catalog. If the tombstone fails after the insert succeeded, the new row is tombstoned best-effort so the catalog does not apply a change the caller was told failed.
  - RENAME TABLE: same `file_id`, new name.
  - ALTER ADD/DROP COLUMN: after the new file is synced, a row with the new `file_id` and schema; the old file is removed last.
  - RENAME COLUMN: same `file_id` and name, new schema; the table's column slice is copied, not edited in place.

A crash leaves only orphans or duplicate rows, which startup (6.3) resolves.

### 6.5 Directory durability

`storage.CreateFile` fsyncs the parent directory after creating the file. Creating a database directory, `data/` or `sys/` fsyncs every directory it created and its parent. All of this happens before the catalog row that depends on it is written. Removing a file needs no directory fsync: a removal that is lost leaves an orphan, which startup removes.

### 6.6 Locking

`Catalog.mu` → `Database.mu` → `Table.mu` → `store.mu`. The store has its own mutex around its two files and its index of row ids; it is held only briefly around one catalog write and never calls back into a database or table.

### 6.7 Config, wiring and shutdown

`data_dir` is already in config and wiring (PR A). C3 adds a Wire provider that returns the catalog together with a cleanup function, so the injector returns `(*grpcserver.Server, func(), error)` and `main` runs the cleanup (`Catalog.Close()`) after `GracefulStop`. `NewCatalog`'s signature does not change.

### 6.8 Tests

- Schema codec: round trip for every data type and constraint; unknown tags error; a golden JSON string pins the on-disk format.
- Store: each operation writes the rows it should; injected I/O failures (a `storage.File` wrapper) leave memory unchanged and new files discarded; wide schema fails before a file exists.
- Restart (through `Engine.Execute` with real SQL): create databases and tables, insert rows, open a fresh catalog on the same directory, compare databases, schemas and rows; repeat after DROP TABLE, DROP DATABASE, RENAME TABLE, ALTER ADD/DROP/RENAME COLUMN.
- Crash states, written directly into the system files: orphan file; orphan directory; table row whose database is gone; duplicate rows after ALTER; duplicate rows after RENAME; the rename-then-alter chain `[a,f1] [b,f1] [a,f2]`; live row with a missing file (must fail startup and close what it opened); torn system file (must fail startup); a case-twin database keeps its files (skipped on a case-sensitive filesystem); ids are never reused across restarts.
- DROP DATABASE followed by CREATE DATABASE of the same name after a failed table-row tombstone does not resurrect the old tables, across a restart.
- `-race` tests stay, including concurrent DDL.

## 7. Limitations accepted

- A torn tail page or corrupt page in a system file makes startup refuse to run; there is no WAL.
- A table schema larger than one page (`storage.MaxRowSize`) cannot be created.
- `UPDATE` (B4) inserts the new version and tombstones the old one under a single `Sync`; a power loss could persist the tombstone without the insert. Not addressed in sub-project 3 (spec 5.4 accepts partial failure without a WAL); the catalog avoids it with two syncs.
- Data directories written by builds before C3 have no catalog, so their table files are removed as orphans at the first start of a C3 build. Those builds never made data durable.
