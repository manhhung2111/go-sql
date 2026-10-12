# Execution engine for CREATE/DROP/SHOW DATABASE

## Context

The parser (`internal/parser/`) is fully built for SELECT/INSERT/UPDATE/DELETE
and CREATE/DROP/SHOW DATABASE (plus CREATE/DROP/ALTER TABLE), and the gRPC
server (`internal/grpcserver/server.go`) currently only checks whether SQL
*parses* — it discards the parsed statement (`_, err := ...Parse()`) and
never runs anything. The user wants to start making queries actually do
something, beginning with the three database-level statements: CREATE
DATABASE, DROP DATABASE, SHOW DATABASES.

The open design question driving this plan: should `SqlStatement` (currently
`type SqlStatement interface{}`, an empty-interface sum type with no methods
anywhere in the AST — same pattern as `Constraint`, `DataType`, `AlterAction`,
`Expression`) grow an `Execute()` method? Confirmed via explicit question:
**no** — add a new `internal/engine` package with an external type-switch
dispatcher instead, matching how `Parse()` itself dispatches and how the
existing test helpers (`dataTypeString`, `constraintStrings`) already do
type-switches over these sum types. This keeps the parser package fully
decoupled from execution/storage.

Second confirmed decision: the gRPC `QueryResponse` currently only carries
`{code, error_message}` — no way to return data, which `SHOW DATABASES`
needs. It gains generic `columns`/`rows` fields (tabular shape) rather than a
field specific to this one statement, so it generalizes to `SELECT` later
without another proto break.

Scope is intentionally limited to these three statements. Every other
`SqlStatement` type (SELECT/INSERT/UPDATE/DELETE/CREATE TABLE/ALTER
TABLE/DROP TABLE) hits a `default` case in the new executor and returns an
error — it must NOT silently report success for statements it doesn't yet
execute, since that would be worse than today's parse-only behavior.

## Approach

### 1. New package `internal/engine/` (two files, one package — matches the
repo's existing "category files within one package" precedent from the
recent `internal/parser` reorg, rather than fragmenting into `storage` +
`engine` sub-packages for just three statements)

**`internal/engine/catalog.go`** — in-memory, concurrency-safe storage:

```go
package engine

import (
	"fmt"
	"sort"
	"sync"
)

type Database struct {
	Name string
}

type Catalog struct {
	mu        sync.RWMutex
	databases map[string]*Database
}

func NewCatalog() *Catalog {
	return &Catalog{databases: make(map[string]*Database)}
}

func (c *Catalog) CreateDatabase(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.databases[name]; exists {
		return fmt.Errorf("database %s already exists", name)
	}
	c.databases[name] = &Database{Name: name}
	return nil
}

func (c *Catalog) DropDatabase(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.databases[name]; !exists {
		return fmt.Errorf("database %s does not exist", name)
	}
	delete(c.databases, name)
	return nil
}

// ListDatabases returns names in sorted order — map iteration order is
// randomized, and callers (and tests) need deterministic output.
func (c *Catalog) ListDatabases() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.databases))
	for name := range c.databases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
```

Database names are stored/compared exactly as written (case-sensitive) —
`internal/parser/lexer.go` only uppercases tokens to classify them against
the keyword table; `Token.Value` on an `IDENT` keeps source casing. So
`CREATE DATABASE Foo` and `DROP DATABASE foo` are different databases,
consistent with the parser's existing no-early-coercion convention.

**`internal/engine/engine.go`** — `Result` type + dispatcher:

```go
package engine

import (
	"fmt"

	"manhhung2111/go-sql/internal/parser"
)

type Result struct {
	Columns []string
	Rows    [][]string
}

type Engine struct {
	catalog *Catalog
}

func NewEngine() *Engine {
	return &Engine{catalog: NewCatalog()}
}

// Execute dispatches on the concrete type of stmt. Only CREATE DATABASE /
// DROP DATABASE / SHOW DATABASES are wired up; every other statement type
// falls into default and returns an error rather than silently no-opping.
// The default case is the seam for future statement types.
func (e *Engine) Execute(stmt parser.SqlStatement) (Result, error) {
	switch s := stmt.(type) {
	case parser.CreateDatabaseStatement:
		if err := e.catalog.CreateDatabase(s.Database); err != nil {
			return Result{}, err
		}
		return Result{}, nil

	case parser.DropDatabaseStatement:
		if err := e.catalog.DropDatabase(s.Database); err != nil {
			return Result{}, err
		}
		return Result{}, nil

	case parser.ShowDatabasesStatement:
		names := e.catalog.ListDatabases()
		rows := make([][]string, len(names))
		for i, name := range names {
			rows[i] = []string{name}
		}
		return Result{Columns: []string{"database"}, Rows: rows}, nil

	default:
		return Result{}, fmt.Errorf("statement not supported, got %T", stmt)
	}
}
```

**`internal/engine/wireset.go`**:

```go
package engine

import "github.com/google/wire"

var WireSet = wire.NewSet(NewEngine)
```

`NewEngine` returns a concrete `*Engine` (no interface) — matches the repo's
existing wiring style; there's no second implementation to justify one.

### 2. Proto changes — `proto/sqlpb/sql.proto`

```protobuf
syntax = "proto3";

package sqlpb;

option go_package = "manhhung2111/go-sql/proto/sqlpb";

message QueryRequest {
  string sql = 1;
}

enum StatusCode {
  OK = 0;
  PARSE_ERROR = 1;
  EXECUTION_ERROR = 2;
}

message Row {
  repeated string values = 1;
}

message QueryResponse {
  StatusCode code = 1;
  string error_message = 2; // empty when code == OK
  repeated string columns = 3; // empty when the statement has no tabular output
  repeated Row rows = 4;       // empty when the statement has no tabular output
}

service SqlParserService {
  rpc ParseQuery(QueryRequest) returns (QueryResponse);
}
```

`Row` is top-level (generates `sqlpb.Row`, not a nested type). Field numbers
1–2 unchanged; 3–4 are additive. Regenerate via `make proto` — this must
happen *before* writing Go code that references `sqlpb.StatusCode_EXECUTION_ERROR`,
`resp.Columns`/`resp.Rows`, or `sqlpb.Row`.

### 3. `internal/grpcserver/server.go`

```go
package grpcserver

import (
	"context"

	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/engine"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/proto/sqlpb"
)

type Server struct {
	sqlpb.UnimplementedSqlParserServiceServer
	config *config.Config
	engine *engine.Engine
}

func NewServer(cfg *config.Config, e *engine.Engine) *Server {
	return &Server{config: cfg, engine: e}
}

func (s *Server) ParseQuery(_ context.Context, req *sqlpb.QueryRequest) (*sqlpb.QueryResponse, error) {
	tokens := parser.NewLexer(req.GetSql()).Lexing()
	stmt, err := parser.NewParser(tokens).Parse()
	if err != nil {
		return &sqlpb.QueryResponse{Code: sqlpb.StatusCode_PARSE_ERROR, ErrorMessage: err.Error()}, nil
	}

	result, err := s.engine.Execute(stmt)
	if err != nil {
		return &sqlpb.QueryResponse{Code: sqlpb.StatusCode_EXECUTION_ERROR, ErrorMessage: err.Error()}, nil
	}

	return &sqlpb.QueryResponse{
		Code:    sqlpb.StatusCode_OK,
		Columns: result.Columns,
		Rows:    toProtoRows(result.Rows),
	}, nil
}

func toProtoRows(rows [][]string) []*sqlpb.Row {
	out := make([]*sqlpb.Row, len(rows))
	for i, row := range rows {
		out[i] = &sqlpb.Row{Values: row}
	}
	return out
}
```

`toProtoRows` stays in `grpcserver` (proto-shape concern), keeping the
`engine` package free of any proto import.

### 4. Wiring

`internal/wiring/wire.go`: add `engine.WireSet` to `WireSet`, add the
`internal/engine` import. `InitializeServer`'s signature is unchanged
(`cfg *config.Config` in, `*grpcserver.Server` in the graph) — wire infers
the new `*engine.Engine` parameter automatically.

Regenerate with `make generate`. If the `wire` binary isn't available,
hand-edit `internal/wiring/wire_gen.go` to the equivalent:

```go
func InitializeServer(cfg *config.Config) (*grpcserver.Server, error) {
	e := engine.NewEngine()
	server := grpcserver.NewServer(cfg, e)
	return server, nil
}
```

`main.go` needs no changes — it already calls `wiring.InitializeServer(cfg)`
exactly once per process, which is what makes the catalog persist across
RPCs (one `Engine`/`Catalog` instance for the server's lifetime).

### 5. `internal/grpcserver/server_test.go` (required changes)

`TestParseQuery_ValidStatements` currently expects `OK` for
SELECT/INSERT/UPDATE/DELETE (lines 46-51) — these become `EXECUTION_ERROR`
once the engine is wired in (they hit `default`). Split into:

- **`TestParseQuery_ValidStatements`**: keep only statements that parse *and*
  execute successfully — `CREATE DATABASE testdb` and `SHOW DATABASES`, plus
  a sequenced case (create then drop against one client/catalog) for
  `DROP DATABASE`.
- **New `TestParseQuery_ExecutionErrors`**: `CREATE DATABASE` twice (→
  `"database testdb already exists"`), `DROP DATABASE` never created (→
  `"database testdb does not exist"`), and one representative unsupported
  statement, e.g. `"SELECT * FROM users"` (→
  `"statement not supported, got parser.SelectStatement"`) — assert
  `StatusCode_EXECUTION_ERROR`.
- **`TestParseQuery_InvalidStatements`**: unchanged (still `PARSE_ERROR`).
- **New test for `SHOW DATABASES` row shape**: create 2+ databases, assert
  `resp.GetColumns() == []string{"database"}` and the rows (flattened via a
  small local helper, mirroring `valueStrings` in
  `internal/parser/testhelpers_test.go`) equal the sorted name list.

`newTestClient` (line 25) changes `NewServer(&config.Config{})` to
`NewServer(&config.Config{}, engine.NewEngine())` — each test gets its own
fresh `Engine`, keeping tests isolated from each other.

### 6. `internal/engine` tests (new)

- `catalog_test.go`: table-driven CRUD on `*Catalog` directly — create
  success/duplicate-error (exact message), drop success/missing-error (exact
  message), `ListDatabases` sorted, `ListDatabases` on empty catalog.
- `engine_test.go`: `Execute` dispatch — one success case per supported
  statement type, one error case per (duplicate create, missing drop), one
  `default`-case test using an unsupported type (e.g.
  `parser.SelectStatement{}`) asserting the exact message.
- Concurrency: a test firing N goroutines doing concurrent
  create/drop/list against one shared `*Catalog`, run manually via
  `go test -race ./internal/engine/` (not added to the Makefile's `test`
  target, which doesn't currently use `-race`).

## Verification

1. `which protoc protoc-gen-go protoc-gen-go-grpc wire` — confirm tooling.
2. Edit `sql.proto`, run `make proto`, then `go build ./proto/...`.
3. Add the three `internal/engine/*.go` files, `go build ./internal/engine/...`.
4. Edit `server.go`, `wiring/wire.go`, run `make generate` (or hand-edit
   `wire_gen.go` per the fallback above).
5. `go build ./...` — full repo compiles.
6. Update `server_test.go`, add `internal/engine/*_test.go`.
7. `go vet ./...`, `go test ./...` (Makefile default target), then
   `go test -race ./internal/engine/`.
8. No `go mod tidy`/`go mod vendor` needed — everything new
   (`sync`/`sort`/`fmt`) is stdlib, no new third-party dependency.

## Out of scope (noted, not built)

- No `Tables` field on `Database` yet — natural extension point once CREATE
  TABLE execution is wired up, not added speculatively now.
- The `default` case in `Engine.Execute` is the seam for every future
  statement type; add one `case` per type as it's implemented.
- Naming debt: `SqlParserService`/`ParseQuery` no longer describe a
  service that now executes statements, not just parses them. Renaming
  ripples through `main.go`, tests, wiring, and both generated `.pb.go`
  files — worth a dedicated follow-up, not bundled into this change.