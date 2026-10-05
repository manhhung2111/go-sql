---
title: A table's rows live only in its file
kind: decision
status: accepted
sources:
  - internal/engine/table.go
  - pr: 6
  - pr: 10
  - pr: 12
updated: 09d8ab0
---
# A table's rows live only in its file

## Context

Tables originally kept an in-memory `Rows` slice. Moving to disk was staged one statement per PR: #6 mirrored INSERT onto the file (a shadow copy with `Rows` still authoritative), the later PRs mirrored ALTER, DELETE and UPDATE, #10 made SELECT read from the file, and #12 deleted `Rows`.

## Decision

`SqlTable.Rows` is gone. `INSERT`, `UPDATE`, `DELETE`, `SELECT` and both ALTER paths read and write only the heap file, under the table's lock, so memory use no longer grows with table size (#12).

## Consequences

- PRIMARY KEY / UNIQUE checks stream the file against a small set of the statement's own key values; time is still one scan per statement. See [full-table scans](../concepts/full-table-scans.md).
- Old error precedence is preserved (earlier-row duplicate beats later-row error; first conflicting unique column in assignment order).
- Two engine-level edge cases changed that SQL cannot reach: an `UPDATE` assigning the same unique column twice is accepted (last value wins), and a closed table with several simultaneous errors may report the closed-table error first (#12).
- Known costs: scans read the whole file; tombstoned space is not reclaimed.

## Alternatives

Not recorded.

## Superseded by

None.
