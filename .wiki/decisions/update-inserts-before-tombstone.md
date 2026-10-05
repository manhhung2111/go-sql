---
title: UPDATE inserts new row versions before tombstoning the old ones
kind: decision
status: accepted
sources:
  - internal/engine/table.go
  - pr: 9
updated: 09d8ab0
---
# UPDATE inserts new row versions before tombstoning the old ones

## Context

Heap pages have no in-place variable-length update; an updated row can change size. With no WAL, a crash in the middle of an `UPDATE` must not lose rows.

## Decision

`Update` scans the file once, encodes and size-checks every new version, inserts the new versions at the end of the file, then tombstones the old ones and fsyncs once (comment on `Update`, #9). A crash between the two writes can duplicate a row but never lose one.

## Consequences

- Updated rows move to the end of the file, so row order after an `UPDATE` is unspecified, and `SELECT` returns them last.
- Each updated row costs an insert plus a tombstone (about three page I/Os) and the old version's space is not reclaimed (#9).
- An `UPDATE` that would push a row past one page is rejected with `row too large` and writes nothing.
- Re-assigning a unique column back to its own current value does not self-conflict: a matched row's own old value is excluded from the conflict check, while two matched rows that would both receive the assigned value do collide.

## Alternatives

Not recorded.

## Superseded by

None.
