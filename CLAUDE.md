# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

go-sql is a hand-written SQL lexer/parser and in-memory execution engine, in Go, exposed over gRPC. The lexer, recursive-descent parser, and storage engine are all built from scratch — nothing here is grammar-generated or backed by an existing database. Go 1.26.4.

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

**Engine** (`internal/engine`): mirrors the SQL hierarchy — `Catalog` (databases) → `Database` (tables) → `Table` (schema + rows) — each a real interface (`SqlCatalog`/`SqlDatabase`/`SqlTable`) with its own `sync.RWMutex` guarding only its own level's state. Reaching into a nested level's data without going through its owning type's locked methods reintroduces the exact concurrency bug each level's lock exists to prevent. `Engine.Execute` type-switches on the parsed `parser.SqlStatement`; that switch is the seam for wiring up a new statement type. An unhandled statement falls into `default` and returns an error rather than silently no-opping — keep it that way when adding new statement types incrementally. This package must never import `proto/sqlpb`; transport-shape conversion (rows → protobuf `Row` messages) belongs in `internal/grpcserver`, keeping the engine transport-agnostic.

- Multi-row/multi-step mutations (`InsertValues`, `AlterColumns`'s `ADD COLUMN`) build into a fresh buffer and only commit in a single final assignment, so a failure partway through never leaves partial state applied.
- `ADD COLUMN`'s backfill of existing rows reuses the same coercion/uniqueness-checking machinery `InsertValues` uses rather than separate rules — follow that precedent for future ALTER-like operations instead of re-deriving the rules.

**gRPC server** (`internal/grpcserver`): a single `ParseQuery(sql, database)` RPC — lex/parse, then execute against the given `database`. A parse failure maps to `PARSE_ERROR` and an execution failure to `EXECUTION_ERROR`; neither ever surfaces as a transport-level gRPC error. See `proto/sqlpb/sql.proto` for the response shape (`columns`/`rows` for tabular output).

## Conventions

- Error messages: lowercase, no trailing punctuation, `%q` for quoted identifiers, "expected X, got Y" phrasing. A few deliberately echo real MySQL wording (e.g. `SHOW DATABASES`'s result column is literally named `"Database"`).
- Tests: testify (`assert`/`require`), table-driven where there's a matrix of cases. Parser tests use canonical-string-rendering helpers (`whereString`, `dataTypeString`, `constraintStrings`, etc., in `testhelpers_test.go`) instead of comparing raw structs, since `Token` carries a `Position` field that would make otherwise-equivalent results compare unequal.
- Commits: Conventional Commits (`<type>(scope): description`).
- `plan/` at the repo root is gitignored — local planning scratch space, never committed.