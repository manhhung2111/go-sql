package parser

type Constraint interface{}

type NotNullConstraint struct{}

type UniqueConstraint struct{}

type PrimaryKeyConstraint struct{}

type DefaultConstraint struct {
	DefaultValue Token
}
