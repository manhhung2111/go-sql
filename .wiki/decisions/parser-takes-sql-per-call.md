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
updated: ccd99ab
---
# Lexer and parser take the SQL per call

## Context

Google Wire builds the dependency graph once at startup, before any request's SQL exists.

## Decision

`NewLexer()` and `NewParser(lexer)` take no runtime arguments; the SQL string is passed per call (`Lexing(sql)`, `Parse(sql)`). This lets both be wired as long-lived singletons (`CLAUDE.md`). Both allocate a fresh `sqlLexer` and `sqlParser` instance per call.

## Consequences

- `Lexing` and `Parse` allocate a fresh `sqlLexer` and `sqlParser` instance per call rather than mutating fields on the shared instance, ensuring that concurrent requests through the shared singletons do not interfere.
- A parser test can call `Parse` repeatedly on one instance.

## Alternatives

Not recorded.

## Superseded by

None.
