---
title: Replace a catalog row by inserting first, then tombstoning
kind: decision
status: accepted
sources:
  - internal/engine/catalogstore.go
  - internal/engine/catalogload.go
  - pr: 15
  - pr: 16
updated: 09d8ab0
---
# Replace a catalog row by inserting first, then tombstoning

## Context

A table rename or an `ALTER` replaces a `sys_tables` row (a new name, or a new `file_id` and schema).

## Decision

`replaceTable` inserts the new row and syncs, then tombstones the old row and syncs again. With a single sync, a power loss could keep the tombstone but not the insert, and the table would vanish from the catalog (comment on `replaceTable`, #15). If the tombstone fails, the new row is tombstoned best-effort so the catalog does not apply a change the caller was told failed.

## Consequences

A crash between the two syncs leaves two rows for one table. Startup resolves this: a later row displaces any current survivor that shares its `(db, name)` or its `file_id` (`catalogStore.load`). Displacement is checked against survivors, not every earlier row: rows `[a,f1] [b,f1] [a,f2]` leave `b` on `f1` and `a` on `f2`, and `f1` must stay because `b` uses it. Known limitation from #16: a failed fsync followed by a failed best-effort cleanup can leave a catalog row for a write that was reported as failed.

## Alternatives

Not recorded.

## Superseded by

None.
