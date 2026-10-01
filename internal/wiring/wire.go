//go:build wireinject
// +build wireinject

//
// go:generate go run github.com/google/wire/cmd/wire

package wiring

import (
	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/engine"
	"manhhung2111/go-sql/internal/grpcserver"
	"manhhung2111/go-sql/internal/parser"

	"github.com/google/wire"
)

var WireSet = wire.NewSet(
	parser.WireSet,
	grpcserver.WireSet,
	engine.WireSet,
	ProvideDataDir,
)

func InitializeServer(cfg *config.Config) (*grpcserver.Server, error) {
	wire.Build(WireSet)
	return nil, nil
}
