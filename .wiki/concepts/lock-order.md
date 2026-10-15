---
title: Lock order
kind: concept
sources:
  - internal/engine/catalog.go
  - internal/engine/database.go
  - internal/engine/table.go
  - internal/engine/catalogstore.go
  - CLAUDE.md
  - pr: 37
updated: 33c2c35
---
# Lock order

## Rule

Acquire locks in this order, never the reverse: `Catalog.mu`, then `Database.mu`, then `Table.mu`, then `tableIndexes.mu`, then `catalogStore.mu`. Each type's `sync.RWMutex` guards only its own level's state. `catalogStore.mu` is a plain `sync.Mutex`.

## Why it matters

Reaching into a nested level's data without going through its owning type's locked methods reintroduces the concurrency bug that each level's lock exists to prevent. `tableIndexes.mu` is a leaf held only while building or discarding index trees. `catalogStore.mu` is last because it is held only around one catalog write and nothing inside it calls back up into a database or table.

## Where it applies

- `SqlCatalog` (`catalog.go`), `SqlDatabase` (`database.go`) and `SqlTable` (`table.go`) each have an `mu sync.RWMutex`.
- `tableIndexes` has its own `mu sync.RWMutex` (leaf).
- `catalogStore` (`catalogstore.go`) has `mu sync.Mutex`.
- A scan takes the table's read lock, so scans of one table may run concurrently; mutations take its write lock.

## How to follow it when adding code

- Go through the owning type's exported or locked methods to touch its state.
- Never call back up the order while holding a lower lock.
- Hold `Table.mu` before calling a `catalogStore` write.

## Verified by

`CLAUDE.md` requires `go test -race ./internal/engine/...` when touching `internal/engine`.
