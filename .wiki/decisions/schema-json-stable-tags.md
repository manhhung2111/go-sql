---
title: Schemas are stored as JSON with stable string tags
kind: decision
status: accepted
sources:
  - internal/engine/schemacodec.go
  - internal/engine/catalogstore.go
  - pr: 14
updated: 09d8ab0
---
# Schemas are stored as JSON with stable string tags

## Context

The catalog must persist each table's columns, types and constraints in the `schema` column of `sys_tables`.

## Decision

`EncodeSchema` / `DecodeSchema` store a table's schema as JSON using stable string tags for every data type and constraint. The parser's own enum values are never stored, because they are iota-numbered and would shift whenever a token is added (comment in `schemacodec.go`, #14). Token positions are dropped. Unknown tags and unknown fields are errors: a schema this build cannot fully understand must not be half-loaded.

## Consequences

- A golden JSON string test pins the on-disk format (#14).
- A new data type or constraint needs an `encodeDataType` / `decodeDataType` or `encodeConstraint` / `decodeConstraint` case, or schema encoding fails for it.
- The schema JSON must fit in one page together with the rest of the `sys_tables` row, so `CREATE TABLE` fails with `schema of table "x" is too large to store` otherwise.
- The catalog's own two tables use hard-coded schemas in `catalogstore.go` (`sysDatabasesColumns`, `sysTablesColumns`): they are the bootstrap every other schema is read through.

## Alternatives

Not recorded.

## Superseded by

None.
