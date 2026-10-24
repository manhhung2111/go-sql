# go-sql

A small SQL database server written from scratch in Go, with no database libraries and no parser generators. The lexer, the recursive-descent parser, the slotted-page storage engine, the B+ tree indexes and the catalog are all hand-written. You talk to it over gRPC.

It is a personal learning project: it works and is tested, but it is not meant for production use (see [Limitations](#limitations)).

## Features

- **SQL:** databases, tables, inserts, selects, updates, deletes, `ALTER TABLE`, and named secondary indexes (see [Supported SQL](#supported-sql)).
- **Durable:** rows live in 16 KiB checksummed pages on disk, and the catalog (databases, tables, index definitions) survives restarts. Every write is fsynced.
- **Indexed:** `PRIMARY KEY` and `UNIQUE` columns, and columns with `CREATE INDEX`, get in-memory B+ trees. Equality and range `WHERE` clauses on them read only the pages that hold matching rows. The trees are saved at shutdown and reloaded at startup, and rebuilt from the data after a crash.
- **Concurrent:** many clients can use the server at once; reads of a table run in parallel and writes are serialized per table.

## Quick start

Start the server (Go 1.26.4):

```
go run ./cmd/server
```

It listens on `127.0.0.1:50051` and keeps its files under `./data`. Stop it with Ctrl-C or SIGTERM; it shuts down gracefully, saving its index snapshots.

Or run it with Docker:

```
docker build -t go-sql .
docker run --rm -p 50051:50051 -v go-sql-data:/data go-sql
```

The container keeps its files in the `/data` volume and runs as a non-root user. `docker stop` sends SIGTERM, which shuts the server down gracefully. CI builds the image on every pull request into `main`, starts it, stops it with SIGTERM and requires a clean exit. Every push to `main` runs the same checks and then publishes the image to Docker Hub as `<user>/go-sql:sha-<short commit sha>` (immutable) and `<user>/go-sql:latest`.

### Try it

The API is one gRPC method, `SqlParserService.ParseQuery(sql, database)` (see [`proto/sqlpb/sql.proto`](proto/sqlpb/sql.proto)). With [grpcurl](https://github.com/fullstorydev/grpcurl):

```
q() { grpcurl -plaintext -proto proto/sqlpb/sql.proto -d "$1" 127.0.0.1:50051 sqlpb.SqlParserService/ParseQuery; }

q '{"sql": "CREATE DATABASE shop"}'
q '{"database": "shop", "sql": "CREATE TABLE users (id INT PRIMARY KEY, email VARCHAR(40) UNIQUE, name VARCHAR(30) NOT NULL)"}'
q '{"database": "shop", "sql": "CREATE INDEX by_name ON users (name)"}'
q '{"database": "shop", "sql": "INSERT INTO users (id, email, name) VALUES (1, \"ada@x.com\", \"Ada\"), (2, \"bob@x.com\", \"Bob\")"}'
q '{"database": "shop", "sql": "SELECT id, name FROM users WHERE name = \"Ada\""}'
```

The last call returns the matching rows as strings:

```
{ "columns": ["id", "name"], "rows": [{ "values": ["1", "Ada"] }] }
```

A statement that fails to parse comes back with `code: PARSE_ERROR`, and one that fails to run (a duplicate key, an unknown table) with `code: EXECUTION_ERROR`; both carry an `errorMessage`. Neither is ever a gRPC transport error. Table statements need the `database` field.

## Supported SQL

| Area | Statements |
| --- | --- |
| Databases | `CREATE DATABASE`, `DROP DATABASE`, `SHOW DATABASES` |
| Tables | `CREATE TABLE [IF NOT EXISTS]`, `DROP TABLE [IF EXISTS]`, `ALTER TABLE` with `ADD COLUMN`, `DROP COLUMN`, `RENAME COLUMN` and `RENAME TO` |
| Indexes | `CREATE INDEX [IF NOT EXISTS] name ON table (column)`, `DROP INDEX [IF EXISTS] name ON table` (non-unique, one column) |
| Rows | `INSERT INTO ... VALUES (...), (...)`, `SELECT cols or * FROM table [WHERE ...]`, `UPDATE ... SET ... [WHERE ...]`, `DELETE FROM table [WHERE ...]` |

- **Types:** `CHAR`, `VARCHAR(n)`, `TEXT`, `BOOLEAN` (or `BOOL`), `SMALLINT`, `MEDIUMINT`, `INT`, `BIGINT`.
- **Constraints:** `PRIMARY KEY` (one column), `UNIQUE`, `NOT NULL`, `DEFAULT`.
- **`WHERE`:** comparisons `=`, `!=`, `<`, `<=`, `>`, `>=` between a column and a literal, combined with `AND` and `OR`.

There are no joins, `ORDER BY`, `LIMIT`, aggregates, transactions or composite keys.

## Configuration

The server reads a YAML file; the path comes from the `-config` flag, then the `CONFIG_PATH` environment variable, then `internal/config/config.yml`:

```yaml
server:
  port: 50051
  host: 127.0.0.1
storage:
  data_dir: ./data
```

## How it is built

```
cmd/server          the server binary
internal/parser     lexer and recursive-descent parser
internal/engine     execution: Catalog -> Database -> Table, catalog persistence
internal/index      key encoding, B+ tree, index snapshots
internal/storage    slotted-page heap files, page files, page cache
internal/grpcserver the ParseQuery RPC
internal/wiring     dependency injection with Google Wire
proto/sqlpb         the gRPC contract
```

Each layer depends only on the one below it. The design notes live in the repository:

- [`CLAUDE.md`](CLAUDE.md): the architecture and conventions in one place.
- [`docs/specs`](docs/specs) and [`docs/plans`](docs/plans): the design and implementation plan behind each feature.
- [`.wiki`](.wiki/index.md): a maintained map of the code, with the reasons behind the main decisions.

## Development

```
go build ./...
go vet ./...
gofmt -l .                  # should print nothing
go test -race ./...         # `make test` runs go test ./...
```

Regenerating the protobuf and Wire code needs `protoc-gen-go`, `protoc-gen-go-grpc` and `wire`, which install under `$(go env GOPATH)/bin`:

```
PATH="$(go env GOPATH)/bin:$PATH" make proto
PATH="$(go env GOPATH)/bin:$PATH" make generate
```

Changes follow plan, then failing test, then implementation; see `CLAUDE.md`.

## Limitations

- **No security:** no TLS and no authentication. Run it on a trusted network or bind it to localhost.
- **No write-ahead log:** a statement interrupted by an I/O error can be left partly applied. A process kill between statements is safe; real power loss has not been tested.
- **Small scale:** tested up to about 100,000 rows per table. An indexed table keeps its keys in memory, and every write fsyncs.
- **Space is not reclaimed:** deleted rows leave their space unused, and a row cannot be larger than one page (about 16 KB).
- **Small SQL surface:** only what is listed above.

## License

MIT, see [LICENSE](LICENSE).
