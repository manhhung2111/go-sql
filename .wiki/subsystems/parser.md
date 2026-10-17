---
title: Parser subsystem
kind: subsystem
sources:
  - internal/parser/
  - pr: 43
updated: ccd99ab
---
# Parser subsystem

The parser is the bottom layer: `internal/parser` turns a SQL string into an AST value and depends on nothing else in the repo. Package map: [internal/parser](../code/parser.md).

## How a statement is parsed

1. `Lexer.Lexing(sql)` scans runes into `Token`s (numbers, single- or double-quoted strings, identifiers and reserved words, symbols) and appends an `EOF` token.
2. `Parser.Parse(sql)` reads the first token and dispatches: SELECT, INSERT INTO, UPDATE, DELETE FROM, CREATE DATABASE, TABLE or INDEX, DROP DATABASE, TABLE or INDEX, SHOW DATABASES, ALTER TABLE. Anything else returns `command not implemented`.
3. The matching `parse*Statement` function builds a statement struct. WHERE clauses are `OR` over `AND` over comparison with operators `=`, `!=`, `<`, `<=`, `>`, `>=` and operands that are identifiers, numbers or strings.
4. `expectEnd` consumes an optional trailing semicolon and rejects any further tokens.

## What this layer promises the next one

- Literal values are raw tokens; see [literal coercion](../concepts/literal-coercion.md).
- Statement, data type, constraint and alter-action values are empty-interface sum types; see [AST sum types](../concepts/ast-sum-types.md).
- The engine's `Execute` type-switch is the only consumer of statements.

## Rules for changes

[Peek before advance](../concepts/peek-before-advance.md). Add a new statement as a struct, a `Parse` case and an `Execute` case together. `Lexing` and `Parse` allocate a fresh lexer and parser instance per call to ensure concurrent safety, see [lexer and parser take the SQL per call](../decisions/parser-takes-sql-per-call.md).
