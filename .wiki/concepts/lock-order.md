---
title: Lock order
kind: concept
sources:
  - internal/engine/catalog.go
  - internal/engine/database.go
  - internal/engine/table.go
  - internal/engine/catalogstore.go
  - CLAUDE.md
updated: 09d8ab0
---
# Lock order

## Rule

Acquire locks in this order, never the reverse: `Catalog.mu`, then `Database.mu`, then `Table.mu`, then `catalogStore.mu`. Each type's `sync.RWMutex` guards only its own level's state. `catalogStore.mu` is a plain `sync.Mutex`.

## Why it matters

Reaching into a nested level's data without going through its owning type's locked methods reintroduces the concurrency bug that each level's lock exists to prevent. `catalogStore.mu` is last because it is held only around one catalog write and nothing inside it calls back up into a database or table (comment on `catalogStore`).

## Where it applies

- `SqlCatalog` (`catalog.go`), `SqlDatabase` (`database.go`) and `SqlTable` (`table.go`) each have an `mu sync.RWMutex`.
- `catalogStore` (`catalogstore.go`) has `mu sync.Mutex`; its methods lock it and never call a database or table.
- A scan takes the table's read lock, so scans of one table may run concurrently; mutations take its write lock. See [storage](../code/storage.md).

## How to follow it when adding code

- Go through the owning type's exported or locked methods to touch its state.
- Never call back up the order while holding a lower lock: nothing in `catalogStore` may call a `Database` or `Table`.
- Hold `Table.mu` before calling a `catalogStore` write, as `Rename` and the ALTER paths do.

## Verified by

`CLAUDE.md` requires `go test -race ./internal/engine/...` when touching `internal/engine`. That finds data races; nothing in the code checks the acquisition order itself.
