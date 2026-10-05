---
title: cmd/server
kind: code
sources:
  - cmd/server/main.go
updated: 09d8ab0
---
# cmd/server

## Purpose

The server binary's entry point: load config, build the server, listen, serve, and shut down gracefully on SIGINT or SIGTERM.

## Depends on / Must not import

Imports `internal/config`, `internal/wiring`, `proto/sqlpb` and `google.golang.org/grpc`.

## Files

| File | Responsibility |
| --- | --- |
| `main.go` | `main` and `envOr` |

## Key types and entry points

`main`:

1. Config path comes from the `-config` flag, defaulting to `$CONFIG_PATH` and then `internal/config/config.yml`.
2. `wiring.InitializeServer(cfg)` returns the server and a cleanup function.
3. Listen on `cfg.Server.Addr()`, register the service, and serve in a goroutine.
4. On a signal: `grpcServer.GracefulStop()`, then `cleanup()`, which closes the catalog's files.

## Called by / calls

Entry point. Calls `config.Load` and `wiring.InitializeServer`.

## Gotchas

- Every exit path after `InitializeServer` runs `cleanup()` (listen failure, serve failure, signal) so file handles are released.
- Run it with `go run ./cmd/server` or `go run ./cmd/server -config <path>`.

## Related

[Request flow](../subsystems/request-flow.md), [wiring](wiring.md), [config](config.md).
