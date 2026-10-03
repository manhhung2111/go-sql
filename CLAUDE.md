# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

go-sql is a hand-written SQL lexer/parser and disk-backed execution engine, in Go, exposed over gRPC. The lexer, recursive-descent parser, and storage engine are all built from scratch — nothing here is grammar-generated or backed by an existing database. Go 1.26.4.

## Commands

```
go build ./...
go vet ./...
go test ./...                                   # or `make test` (default Make target)
go test ./internal/engine/... -run TestName -v  # single test
go test -race ./internal/engine/...             # required when touching internal/engine (concurrent Catalog/Database/Table)
gofmt -l .                                      # should report nothing; gofmt -w <file> to fix
```

`protoc-gen-go`, `protoc-gen-go-grpc`, and `wire` install under `$(go env GOPATH)/bin`, which isn't on `PATH` in a fresh shell:

```
PATH="$(go env GOPATH)/bin:$PATH" make proto      # regenerate proto/sqlpb/*.pb.go from sql.proto
PATH="$(go env GOPATH)/bin:$PATH" make generate    # regenerate internal/wiring/wire_gen.go from wire.go
```

Run `make proto` before writing Go code against a new/changed proto field — otherwise it doesn't exist yet.

```
go run ./cmd/server                    # reads internal/config/config.yml by default
go run ./cmd/server -config <path>      # or set CONFIG_PATH env var
```

`vendor/` is populated locally (`go mod vendor`) but gitignored, not committed.

## Architecture

Three layers, each depending only on the one below it:

`internal/parser` (lexer + recursive-descent parser) → `internal/engine` (execution: `Catalog` → `Database` → `Table`) → `internal/grpcserver` (single gRPC method `ParseQuery`), wired together with Google Wire in `internal/wiring`.

**Parser** (`internal/parser`): `Lexer`/`Parser` are constructed with no runtime args (`NewLexer()`, `NewParser(lexer)`) and take the SQL string per call (`Lexing(sql)`, `Parse(sql)`) rather than at construction — this is what lets both be wired as long-lived singletons instead of needing the request's SQL at graph-build time. `SqlStatement`, `DataType`, `Constraint`, and `AlterAction` are all empty-interface sum types (`type X interface{}`) with concrete struct variants — there are no methods on the AST node types. Every consumer (`Parse()`'s own dispatch, `coerceValue`, `Engine.Execute`) is an external function doing a Go type-switch; follow that pattern for anything new rather than adding a method to an AST struct.

- Always `peek()` and check a token's type before `advance()`ing past it. Advancing first and validating after was the root cause of past bugs (e.g. a constraint-parsing loop that silently discarded tokens).
- Literal values stay as a raw `Token` through parsing. Coercion to a native Go type happens only in `internal/engine` (`coerceValue`), once the target column's `DataType` is known — don't add early type conversion in the parser.

**Engine** (`internal/engine`): mirrors the SQL hierarchy — `Catalog` (databases) → `Database` (tables) → `Table` (schema + its heap file) — each a real interface (`SqlCatalog`/`SqlDatabase`/`SqlTable`) with its own `sync.RWMutex` guarding only its own level's state. Lock order is `Catalog.mu` → `Database.mu` → `Table.mu` → `catalogStore.mu` (the last is held only around one catalog write and never calls back up). Reaching into a nested level's data without going through its owning type's locked methods reintroduces the exact concurrency bug each level's lock exists to prevent. `Engine.Execute` type-switches on the parsed `parser.SqlStatement`; that switch is the seam for wiring up a new statement type. An unhandled statement falls into `default` and returns an error rather than silently no-opping — keep it that way when adding new statement types incrementally. This package must never import `proto/sqlpb`; transport-shape conversion (rows → protobuf `Row` messages) belongs in `internal/grpcserver`, keeping the engine transport-agnostic.

- Multi-row/multi-step mutations (`InsertValues`, `Update`, `AlterColumns`'s `ADD`/`DROP COLUMN`) validate and encode everything before the first write to the table's file, so a bad statement writes nothing; only an I/O error can leave one partly applied, since there is no WAL.
- `ADD COLUMN`'s backfill of existing rows reuses the same coercion/uniqueness-checking machinery `InsertValues` uses rather than separate rules — follow that precedent for future ALTER-like operations instead of re-deriving the rules.

**gRPC server** (`internal/grpcserver`): a single `ParseQuery(sql, database)` RPC — lex/parse, then execute against the given `database`. A parse failure maps to `PARSE_ERROR` and an execution failure to `EXECUTION_ERROR`; neither ever surfaces as a transport-level gRPC error. See `proto/sqlpb/sql.proto` for the response shape (`columns`/`rows` for tabular output).

**Storage** (`internal/storage`, standalone): slotted-page heap files of 16KiB checksummed pages, with tombstone deletes and a checksum-verifying `Scan`. It deals only in `[]byte` rows and never imports `internal/parser`. The engine owns one file per `SqlTable` at `<storage.data_dir>/data/<db>/<file_id>.tbl` (default `data`, gitignored). Files are named by numeric id, never by table name, so renaming a table never touches disk; ids come from `fileAllocator` and are never reused within a process. `Catalog`/`Database`/`Table` all have `Close()`; `Database`/`Table` have `Drop()`. A table's rows live only in its file; no statement keeps them in memory. INSERT appends; SELECT streams the file a page at a time under the table's read lock (scans may run concurrently with one another); DELETE tombstones the matching rows; UPDATE inserts each new row version at the end of the file and tombstones the old one, so updated rows move to the end and row order after an UPDATE is unspecified; ALTER ADD/DROP COLUMN streams the file into a new file (a fresh `file_id`, the old one deleted) in the new column layout, and RENAME COLUMN needs no rewrite because rows are positional. PRIMARY KEY/UNIQUE checks stream the file against a small set of the statement's own key values, so memory is proportional to the statement and time to the table until an index exists. The space of deleted rows is not reclaimed yet. A row whose encoding exceeds one page (`storage.MaxRowSize`) is rejected with `row too large`, since there are no overflow pages.

The catalog is durable too: two system heap files under `<storage.data_dir>/sys/` (`sys_databases`, `sys_tables(db, name, file_id, schema)`) hold every database and table, with schemas as JSON (`schemacodec.go`, stable string tags, never the parser's enum values). Every DDL statement writes its catalog row and fsyncs before changing memory, so the catalog row is the commit point; replacing a row (rename, ALTER) is insert + sync, then tombstone + sync. At startup `catalogStore.load` resolves duplicate rows left by a crash against the current survivors (a later row displaces a survivor with the same `(db, name)` or `file_id`), opens every table's file, and `removeOrphans` deletes `<id>.tbl` files no survivor references — decided by id, never by directory name, never recursively, and only files named `<number>.tbl`. A live row whose file is missing or torn, or a damaged system file, refuses startup. `CreateFile` and directory creation fsync the parent directory. `Catalog.Close()` runs on shutdown through the Wire cleanup returned by `InitializeServer`. A table whose schema JSON does not fit one page cannot be created (`schema of table … is too large to store`).

## Conventions

- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers, "expected X, got Y" phrasing. A few deliberately echo real MySQL wording (e.g. `SHOW DATABASES`'s result column is literally named `"Database"`).
- Tests: testify (`assert`/`require`), table-driven where there's a matrix of cases. Parser tests use canonical-string-rendering helpers (`whereString`, `dataTypeString`, `constraintStrings`, etc., in `testhelpers_test.go`) instead of comparing raw structs, since `Token` carries a `Position` field that would make otherwise-equivalent results compare unequal.
- Commits: Conventional Commits (`<type>(scope): description`).
- `plan/` at the repo root is gitignored — local planning scratch space, never committed.