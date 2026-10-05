---
title: internal/grpcserver
kind: code
sources:
  - internal/grpcserver/
  - proto/sqlpb/sql.proto
updated: 09d8ab0
---
# internal/grpcserver

## Purpose

The gRPC transport: one RPC, `ParseQuery`, that lexes and parses the SQL and then executes it against the requested database.

## Depends on / Must not import

Imports `internal/config`, `internal/engine`, `internal/parser` and `proto/sqlpb`. The engine does not import this package or `sqlpb`.

## Files

| File | Responsibility |
| --- | --- |
| `server.go` | `Server`, `NewServer`, `ParseQuery`, `toProtoRows` |
| `wireset.go` | `WireSet` providing `NewServer` |

## Key types and entry points

`Server.ParseQuery(ctx, *sqlpb.QueryRequest) (*sqlpb.QueryResponse, error)`:

1. `parser.Parse(req.GetSql())`; a failure returns `Code: PARSE_ERROR` with the error text.
2. `engine.Execute(statement, req.GetDatabase())`; a failure returns `Code: EXECUTION_ERROR`.
3. Otherwise `Code: OK` with `Columns` and rows converted by `toProtoRows`.

Both error kinds are returned in the response with a nil Go error, so they never become transport-level gRPC errors. See [errors in the response body](../decisions/grpc-errors-in-response-body.md).

## Called by / calls

Registered by `cmd/server` through `sqlpb.RegisterSqlParserServiceServer`. Calls `Parser.Parse` and `Engine.Execute`.

## Gotchas

- `Server` holds one `parser.Parser` for all requests, and the parser keeps per-call state on itself. See [parser](parser.md).
- `Server` stores `config` but `ParseQuery` does not use it.
- The service is named `SqlParserService` in the proto even though `ParseQuery` also executes statements.

## Related

[Request flow](../subsystems/request-flow.md), [proto](proto.md).
