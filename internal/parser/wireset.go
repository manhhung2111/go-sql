package parser

import "github.com/google/wire"

var WireSet = wire.NewSet(
	NewLexer,
	NewParser,
)
