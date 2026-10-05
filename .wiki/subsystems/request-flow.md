---
title: Request flow subsystem
kind: subsystem
sources:
  - cmd/server/main.go
  - internal/wiring/
  - internal/grpcserver/server.go
  - proto/sqlpb/sql.proto
  - internal/config/
updated: 09d8ab0
---
# Request flow subsystem

How the server starts and how one query travels through the three layers: parser, engine, gRPC server, wired together with Google Wire.

## Startup

[`cmd/server`](../code/cmd-server.md) loads the config ([`internal/config`](../code/config.md): the `-config` flag, then `CONFIG_PATH`, then `internal/config/config.yml`), then calls `wiring.InitializeServer(cfg)` ([wiring](../code/wiring.md)). Wire builds the lexer, parser, catalog (via `ProvideCatalog`, which loads the persisted catalog), engine and gRPC `Server`, and returns a cleanup function that closes the catalog.

## One request

1. The client calls `SqlParserService.ParseQuery(sql, database)` ([proto](../code/proto.md)).
2. `Server.ParseQuery` parses the SQL. A parse failure becomes `PARSE_ERROR` in the response.
3. It calls `Engine.Execute(statement, database)`. A failure becomes `EXECUTION_ERROR` in the response.
4. On success, the engine's `Response` rows are converted to protobuf `Row`s by `toProtoRows` and returned with `Code: OK`.

Neither failure ever becomes a transport-level gRPC error: [errors in the response body](../decisions/grpc-errors-in-response-body.md). The engine stays transport-agnostic because it never imports `sqlpb`.

## Shutdown

On SIGINT or SIGTERM, `main` calls `GracefulStop()` and then the cleanup function.

## Watch out for

The server shares one parser instance across requests: [lexer and parser take the SQL per call](../decisions/parser-takes-sql-per-call.md).
