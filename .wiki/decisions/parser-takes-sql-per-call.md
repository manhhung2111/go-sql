---
title: Lexer and parser take the SQL per call
kind: decision
status: accepted
sources:
  - internal/parser/parser.go
  - internal/parser/lexer.go
  - internal/parser/wireset.go
  - internal/wiring/wire.go
  - CLAUDE.md
  - pr: 46
updated: 01306f5
---
# Lexer and parser take the SQL per call

## Context

Google Wire builds the dependency graph once at startup, before any request's SQL exists.

## Decision

`NewLexer()` and `NewParser(lexer)` take no runtime arguments; the SQL string is passed per call (`Lexing(sql)`, `Parse(sql)`). This lets both be wired as long-lived singletons (`CLAUDE.md`). To ensure thread safety across concurrent requests, `Lexing` and `Parse` allocate fresh `sqlLexer` and `sqlParser` values per call, so the shared singletons hold no scan or parse state (#46).

## Consequences

- The shared `Lexer` and `Parser` singletons are safe for concurrent use across gRPC requests without locking or request serialization (#46).
- Unit and gRPC server integration tests verify that concurrent `Lexing` and `Parse` calls execute safely without data races or state corruption (`concurrency_test.go`, `server_test.go`, #46).
- A parser test can call `Parse` repeatedly on one instance.

## Alternatives

Not recorded.

## Superseded by

None.
