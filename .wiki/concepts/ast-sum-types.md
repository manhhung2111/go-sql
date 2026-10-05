---
title: AST sum types and type-switch dispatch
kind: concept
sources:
  - internal/parser/parser.go
  - internal/parser/constraints.go
  - internal/parser/datatypes.go
  - internal/parser/ast_ddl.go
  - internal/engine/engine.go
  - CLAUDE.md
updated: 09d8ab0
---
# AST sum types and type-switch dispatch

## Rule

`SqlStatement`, `DataType`, `Constraint`, `AlterAction` and `Expression` are empty interfaces (`type X interface{}`) with concrete struct variants. The AST node types have no methods that carry behaviour. Every consumer is an external function doing a Go type-switch; follow that for anything new instead of adding a method to an AST struct.

## Why it matters

The seam for a new statement is the type-switch in `Engine.Execute`. An unhandled statement type falls into `default` and returns an error (`statement not supported, got %T`) instead of silently doing nothing; this keeps incremental addition of statement types safe.

## Where it applies

- `parser.Parse` dispatches on the first token, then on the next keyword for CREATE, DROP, SHOW and ALTER.
- `Engine.Execute` switches on the statement type; `AlterTableStatement` additionally checks for `RenameTableAction` (handled at the database level) before delegating other actions to the table.
- `coerceValue`, `kindOf`, `encodeDataType` / `decodeDataType` and `encodeConstraint` / `decodeConstraint` switch on data type and constraint variants.
- The few helpers on AST structs are accessors such as `ColumnDefinition.IsPrimaryKey()`, `IsNotNull()`, `IsUnique()`, `RequiresValue()` and `DefaultValue()`.

## How to follow it when adding code

Add a struct variant in `internal/parser`, a case in `Parse`, and a case in the consumer's switch (`Execute`, `coerceValue`, the codecs). Keep `default` returning an error.

## Verified by

Parser tests and engine tests per statement. No compile-time exhaustiveness check exists because the interfaces are empty.
