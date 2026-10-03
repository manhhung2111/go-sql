package wiring

import (
	"log"

	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/engine"
)

// ProvideDataDir hands the configured data directory to the engine.
func ProvideDataDir(cfg *config.Config) engine.DataDir {
	return engine.DataDir(cfg.Storage.DataDir)
}

// ProvideCatalog opens the catalog and returns the function that closes it, so
// the server releases its file handles on shutdown.
func ProvideCatalog(dataDir engine.DataDir) (engine.Catalog, func(), error) {
	catalog, err := engine.NewCatalog(dataDir)
	if err != nil {
		return nil, nil, err
	}
	return catalog, func() {
		if err := catalog.Close(); err != nil {
			log.Printf("closing catalog: %v", err)
		}
	}, nil
}
