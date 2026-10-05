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
updated: 09d8ab0
---
# Lexer and parser take the SQL per call

## Context

Google Wire builds the dependency graph once at startup, before any request's SQL exists.

## Decision

`NewLexer()` and `NewParser(lexer)` take no runtime arguments; the SQL string is passed per call (`Lexing(sql)`, `Parse(sql)`). This lets both be wired as long-lived singletons (`CLAUDE.md`). Both reset their scan state at the start of each call (`l.pos = 0`, `s.pos = 0`).

## Consequences

- `sqlParser` keeps `tokens` and `pos` on the struct and `sqlLexer` keeps `runes` and `pos`, so a single instance holds per-request state. The code has no synchronization, and `grpcserver.Server` uses one parser for every request; concurrent `Parse` calls would share that state. No source records whether this was considered.
- A parser test can call `Parse` repeatedly on one instance.

## Alternatives

Not recorded.

## Superseded by

None.
