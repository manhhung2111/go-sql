package engine

import "fmt"

type Response struct {
	Columns []string
	Rows    [][]string
}

// stringifyValue renders a coerced native column value (int64, string,
// bool, or nil for SQL NULL) as the string Response.Rows expects.
func stringifyValue(v any) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%v", v)
}
