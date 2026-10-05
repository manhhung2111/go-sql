# Wiki index

Read this first. Conventions are in [SCHEMA.md](SCHEMA.md); the ingest history and marker are in [log.md](log.md). Validate with `.wiki/check.sh`.

## Subsystems

- [Parser subsystem](subsystems/parser.md): how a SQL string becomes a statement value
- [Engine subsystem](subsystems/engine.md): Catalog, Database and Table execution, and the properties every statement must keep
- [Storage subsystem](subsystems/storage.md): 16KiB slotted-page heap files, their write and read paths and limits
- [Catalog persistence subsystem](subsystems/catalog-persistence.md): system files, write-ahead ordering, startup load and recovery
- [Request flow subsystem](subsystems/request-flow.md): startup, one gRPC request end to end, shutdown
- [Build, CI and deployment subsystem](subsystems/deployment.md): Docker image, CI workflows, make targets

## Code map

- [internal/parser](code/parser.md): lexer, recursive-descent parser, AST
- [internal/engine](code/engine.md): execution engine, catalog, codecs
- [internal/engine/table.go](code/engine-table.md): the table implementation (largest file)
- [internal/storage](code/storage.md): heap files and pages
- [internal/grpcserver](code/grpcserver.md): the ParseQuery RPC
- [internal/wiring](code/wiring.md): Wire injector and providers
- [internal/config](code/config.md): YAML server config
- [cmd/server](code/cmd-server.md): the server binary
- [proto/sqlpb](code/proto.md): the gRPC contract

## Concepts

- [Lock order](concepts/lock-order.md): Catalog, Database, Table, then catalogStore
- [Validate everything, then write](concepts/validate-then-write.md): bad statements write nothing
- [Literal coercion happens only in the engine](concepts/literal-coercion.md): parser keeps raw tokens
- [AST sum types and type-switch dispatch](concepts/ast-sum-types.md): empty interfaces, external switches
- [Full-table scans until an index exists](concepts/full-table-scans.md): cost model of every lookup
- [Peek before advance in the parser](concepts/peek-before-advance.md): the parser's token rule

## Decisions

- [Catalog row is the commit point](decisions/catalog-row-is-the-commit-point.md): DDL writes and fsyncs its catalog row first
- [Replace a catalog row by inserting first, then tombstoning](decisions/replace-row-insert-then-tombstone.md): rename and ALTER ordering
- [Table files are named by numeric id](decisions/file-ids-not-names.md): renames never touch disk
- [A table's rows live only in its file](decisions/rows-only-in-file.md): the in-memory rows slice is gone
- [Deletes tombstone a slot and space is not reclaimed](decisions/tombstone-deletes-no-reclaim.md): stable slot indices
- [Schemas are stored as JSON with stable string tags](decisions/schema-json-stable-tags.md): never the parser's enum values
- [UPDATE inserts new row versions before tombstoning the old ones](decisions/update-inserts-before-tombstone.md): a crash can duplicate, not lose
- [Orphan files are removed by id, never by directory name](decisions/orphan-removal-by-file-id.md): safe on case-insensitive filesystems
- [Lexer and parser take the SQL per call](decisions/parser-takes-sql-per-call.md): lets Wire build singletons
- [Parse and execution errors are returned in the response body](decisions/grpc-errors-in-response-body.md): never a transport-level error
