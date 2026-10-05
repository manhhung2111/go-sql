---
title: Literal coercion happens only in the engine
kind: concept
sources:
  - internal/engine/coerce.go
  - internal/parser/token.go
  - internal/engine/where.go
  - CLAUDE.md
updated: 09d8ab0
---
# Literal coercion happens only in the engine

## Rule

The parser keeps a literal value as a raw `Token{Type, Value, Position}`. Conversion to a native Go value happens only in `internal/engine`, in `coerceValue`, once the target column's `DataType` is known. Do not add early type conversion to the parser.

## Why it matters

The type of a literal depends on the column it is compared with or stored in, which only the engine knows. `Token` carries a `Position`, which is also why parser tests compare canonical string renderings instead of raw structs (see `CLAUDE.md`).

## Where it applies

- `coerceValue(dt, value)` passes `nil` (SQL NULL) through, then dispatches on the data type: `coerceString` (a max-length check from the type's size, no padding, for CHAR, VARCHAR and TEXT), `coerceInt` (the SQL type's own fixed range, independent of display width), `coerceBoolean` (NUMBER 0/1 or STRING `true`/`false`, case-insensitive).
- `InsertValues` and `Update` coerce against the column's type; DEFAULT values are coerced eagerly when the column is validated.
- `where.go`'s `resolveOperand` defers coercion of a literal until the other side's type is known; two literals compared with each other go through `coerceLiteral` (NUMBER becomes `int64`, STRING stays raw).

## How to follow it when adding code

A new literal-consuming path should take `parser.Token` values from the AST and call `coerceValue` with the destination `DataType`. A new data type needs a case in `coerceValue`, in `kindOf` and the row codec, and in the JSON schema codec.

## Verified by

Engine tests for coercion errors; there is no check that the parser stays free of conversions.
