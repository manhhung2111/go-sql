---
title: internal/config
kind: code
sources:
  - internal/config/
  - deploy/config.docker.yml
updated: 09d8ab0
---
# internal/config

## Purpose

Loads the server configuration from a YAML file.

## Depends on / Must not import

Imports `go.yaml.in/yaml/v3` and the standard library only.

## Files

| File | Responsibility |
| --- | --- |
| `config.go` | `Config{Server, Storage}`, `ServerConfig{Host, Port}` with `Addr()`, `StorageConfig{DataDir}`, `DefaultDataDir`, `Load(path)` |
| `config.yml` | Default config: `127.0.0.1:50051`, `data_dir: ./data` |

## Key types and entry points

`Load(path string) (*Config, error)` reads and parses the file, and sets `Storage.DataDir` to `DefaultDataDir` (`"data"`, relative to the server's working directory) when it is empty.

## Called by / calls

Called by `cmd/server`. Its `Config` is passed to `wiring.InitializeServer`.

## Gotchas

- `deploy/config.docker.yml` is a separate file that binds `0.0.0.0:50051` and uses `/data`; the Docker image sets `CONFIG_PATH` to it. The repo's own `config.yml` keeps a loopback bind and a relative data dir so local `go run` is unchanged.

## Related

[Deployment](../subsystems/deployment.md), [cmd/server](cmd-server.md).
