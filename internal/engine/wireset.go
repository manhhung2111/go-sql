package engine

import "github.com/google/wire"

var WireSet = wire.NewSet(
	NewEngine,
)
