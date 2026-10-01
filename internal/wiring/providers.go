package wiring

import (
	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/engine"
)

// ProvideDataDir hands the configured data directory to the engine.
func ProvideDataDir(cfg *config.Config) engine.DataDir {
	return engine.DataDir(cfg.Storage.DataDir)
}
