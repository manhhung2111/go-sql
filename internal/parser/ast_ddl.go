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

func hasConstraint[T Constraint](c ColumnDefinition) bool {
	for _, constraint := range c.Constraints {
		if _, ok := constraint.(T); ok {
			return true
		}
	}
	return false
}

func (c ColumnDefinition) IsPrimaryKey() bool { return hasConstraint[PrimaryKeyConstraint](c) }
func (c ColumnDefinition) IsNotNull() bool    { return hasConstraint[NotNullConstraint](c) }
func (c ColumnDefinition) IsUnique() bool     { return hasConstraint[UniqueConstraint](c) }

// RequiresValue reports whether a column must never be NULL — true for an
// explicit NOT NULL constraint, and also for PRIMARY KEY, which implies
// NOT NULL even without stating it separately.
func (c ColumnDefinition) RequiresValue() bool { return c.IsNotNull() || c.IsPrimaryKey() }

func (c ColumnDefinition) DefaultValue() (Token, bool) {
	for _, constraint := range c.Constraints {
		if d, ok := constraint.(DefaultConstraint); ok {
			return d.DefaultValue, true
		}
	}
	return Token{}, false
}
