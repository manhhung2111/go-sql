package parser

type DataType interface{}

type CharDataType struct {
	Size int
}

type VarCharDataType struct {
	Size int
}

type TextDataType struct {
	Size int
}

type BooleanDataType struct{}

type SmallIntDataType struct {
	Size int
}

type MediumIntDataType struct {
	Size int
}

type IntDataType struct {
	Size int
}

type BigIntDataType struct {
}
