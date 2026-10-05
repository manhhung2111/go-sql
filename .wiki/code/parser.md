---
title: internal/parser
kind: code
sources:
  - internal/parser/
updated: 09d8ab0
---
# internal/parser

## Purpose

A hand-written lexer and recursive-descent parser. It turns one SQL string into a `SqlStatement` value. Nothing is grammar-generated.

## Depends on / Must not import

Standard library plus `github.com/google/wire` (only in `wireset.go`). It never imports `internal/engine`.

## Files

| File | Responsibility |
| --- | --- |
| `lexer.go` | `Lexer` interface, `NewLexer`, `Lexing(sql) []Token`; scans numbers, quoted strings, identifiers, symbols |
| `token.go` | `TokenType` constants, reserved-word map, `Token{Type, Value, Position}` |
| `parser.go` | `Parser` interface, `NewParser`, `Parse`: dispatch on the first token, then `expectEnd` |
| `parser_ddl.go` | CREATE/DROP DATABASE, SHOW DATABASES, CREATE/DROP/ALTER TABLE, column definitions and constraints |
| `parser_dml.go` | SELECT, INSERT INTO, UPDATE, DELETE |
| `expression.go` | WHERE grammar: OR over AND over comparison; `BinaryExpression`, `ComparisonExpression` |
| `ast_ddl.go`, `ast_dml.go` | Statement structs and `ColumnDefinition` (with `IsPrimaryKey`, `IsNotNull`, `IsUnique`, `RequiresValue`, `DefaultValue`) |
| `constraints.go`, `datatypes.go` | Constraint and data type structs |
| `wireset.go` | `WireSet` providing `NewLexer` and `NewParser` |

## Key types and entry points

- `Parser.Parse(sql string) (SqlStatement, error)` in `parser.go`: the only entry point the rest of the repo uses.
- `SqlStatement`, `DataType`, `Constraint`, `AlterAction` and `Expression` are all empty interfaces; see [AST sum types](../concepts/ast-sum-types.md).
- Statement structs: `CreateDatabaseStatement`, `DropDatabaseStatement`, `ShowDatabasesStatement`, `CreateTableStatement`, `DropTableStatement`, `AlterTableStatement` (with `AddColumnAction`, `DropColumnAction`, `RenameColumnAction`, `RenameTableAction`), `SelectStatement`, `InsertIntoStatement`, `UpdateStatement`, `DeleteStatement`.

## Called by / calls

`internal/grpcserver` calls `Parse`. Within the package, `Parse` calls `Lexer.Lexing` and then the `parse*Statement` functions.

## Gotchas

- `sqlParser` stores `tokens` and `pos` on the struct and `sqlLexer` stores `runes` and `pos`; `Parse` and `Lexing` reset them on each call. The code has no locking, and `grpcserver.Server` holds a single parser instance, so concurrent `Parse` calls would share that state. See [SQL passed per call](../decisions/parser-takes-sql-per-call.md).
- Literal values stay raw `Token`s; the engine coerces them. See [literal coercion](../concepts/literal-coercion.md).
- `Lexing` has a `TODO: handle negative number` comment, so negative numeric literals are not handled.
- `Parse` rejects tokens left after an optional trailing semicolon (`expectEnd`).
- Parser code must `peek()` and check a token's type before `advance()`; see [peek before advance](../concepts/peek-before-advance.md).

## Related

[Parser subsystem](../subsystems/parser.md), [engine](engine.md).
