---
title: Parse and execution errors are returned in the response body
kind: decision
status: accepted
sources:
  - internal/grpcserver/server.go
  - proto/sqlpb/sql.proto
  - CLAUDE.md
updated: 09d8ab0
---
# Parse and execution errors are returned in the response body

## Context

`ParseQuery` can fail in the parser or in the engine; either failure is a normal outcome for a SQL client.

## Decision

A parse failure returns `QueryResponse{Code: PARSE_ERROR, ErrorMessage: ...}` and an execution failure returns `Code: EXECUTION_ERROR`; both return a nil Go error, so neither ever surfaces as a transport-level gRPC error (`server.go`, `CLAUDE.md`). Success is `Code: OK` with `Columns` and `Rows`.

## Consequences

- A client must read `code` and `error_message` from the response; a successful gRPC call does not mean the statement succeeded.
- `error_message` is empty when `code == OK` (proto comment).

## Alternatives

Not recorded.

## Superseded by

None.
