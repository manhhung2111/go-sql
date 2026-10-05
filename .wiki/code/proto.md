---
title: proto/sqlpb
kind: code
sources:
  - proto/sqlpb/sql.proto
  - Makefile
updated: 09d8ab0
---
# proto/sqlpb

## Purpose

The gRPC contract. `sql.proto` is the source; `sql.pb.go` and `sql_grpc.pb.go` are generated from it.

## Depends on / Must not import

Generated code only. `internal/engine`, `internal/storage` and `internal/parser` do not import it.

## Files

| File | Responsibility |
| --- | --- |
| `sql.proto` | Messages `QueryRequest`, `Row`, `QueryResponse`; enum `StatusCode`; service `SqlParserService` |
| `sql.pb.go`, `sql_grpc.pb.go` | Generated; do not edit by hand |

## Key types and entry points

- `QueryRequest{sql, database}`: `database` is required for table-level statements.
- `QueryResponse{code, error_message, columns, rows}`: `error_message` is empty when `code == OK`; `columns` and `rows` are empty when the statement has no tabular output.
- `StatusCode`: `OK = 0`, `PARSE_ERROR = 1`, `EXECUTION_ERROR = 2`.
- `Row{values}`: every value is a string.

## Called by / calls

Used by `internal/grpcserver` and `cmd/server`.

## Gotchas

- Regenerate with `PATH="$(go env GOPATH)/bin:$PATH" make proto` after editing the proto, and before writing Go code against a new field.
- `protoc-gen-go` and `protoc-gen-go-grpc` install under `$(go env GOPATH)/bin`, which is not on `PATH` in a fresh shell.

## Related

[grpcserver](grpcserver.md), [Request flow](../subsystems/request-flow.md).
