package parser

type CreateDatabaseStatement struct {
	Database string
}

type DropDatabaseStatement struct {
	Database string
}

type DropTableStatement struct {
	Table    string
	IfExists bool
}

type ShowDatabasesStatement struct{}

type CreateTableStatement struct {
	Table       string
	IfNotExists bool
	Columns     []ColumnDefinition
}

type ColumnDefinition struct {
	Name        string
	DataType    DataType
	Constraints []Constraint
}

type AlterTableStatement struct {
	Table  string
	Action AlterAction
}

type AlterAction interface{}

type AddColumnAction struct {
	Column ColumnDefinition
}

type DropColumnAction struct {
	Column string
}

type RenameColumnAction struct {
	OldName string
	NewName string
}

type RenameTableAction struct {
	NewName string
}
