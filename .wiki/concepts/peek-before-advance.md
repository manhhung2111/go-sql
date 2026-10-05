---
title: Peek before advance in the parser
kind: concept
sources:
  - internal/parser/parser.go
  - internal/parser/parser_ddl.go
  - CLAUDE.md
updated: 09d8ab0
---
# Peek before advance in the parser

## Rule

Always `peek()` and check a token's type before `advance()`ing past it. Do not advance first and validate afterwards.

## Why it matters

`CLAUDE.md` records that advancing first and validating after was the root cause of past bugs, for example a constraint-parsing loop that silently discarded tokens. A related guard in `parseColumnConstraints` stops at the first token that is not a constraint keyword without consuming it.

## Where it applies

The helpers `peek()`, `advance()` and `expect(tokenType)` in `parser.go`, and every `parse*` function in `parser_ddl.go` and `parser_dml.go`. `expect` consumes a token only if it has the wanted type.

## How to follow it when adding code

Branch on `s.peek().Type` or call `s.expect(...)`; call `advance()` only after you know the token belongs to the rule you are parsing.

## Verified by

Parser tests for malformed statements. No static check enforces it.
