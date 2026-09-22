package parser

type SelectStatement struct {
	Columns []string
	Table   string
	Where   Expression
}

type InsertIntoStatement struct {
	Columns []string
	Table   string
	Values  [][]Token
}

type Assignment struct {
	Column string
	Value  Token
}

type UpdateStatement struct {
	Table string
	Set   []Assignment
	Where Expression
}

type DeleteStatement struct {
	Table string
	Where Expression
}
