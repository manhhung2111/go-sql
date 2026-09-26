package grpcserver

import (
	"context"
	"manhhung2111/go-sql/internal/engine"

	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/proto/sqlpb"
)

type Server struct {
	sqlpb.UnimplementedSqlParserServiceServer
	config *config.Config
	parser parser.Parser
	engine engine.Engine
}

func NewServer(cfg *config.Config, parser parser.Parser, engine engine.Engine) *Server {
	return &Server{config: cfg, parser: parser, engine: engine}
}

func (s *Server) ParseQuery(_ context.Context, req *sqlpb.QueryRequest) (*sqlpb.QueryResponse, error) {
	statement, err := s.parser.Parse(req.GetSql())
	if err != nil {
		return &sqlpb.QueryResponse{Code: sqlpb.StatusCode_PARSE_ERROR, ErrorMessage: err.Error()}, nil
	}

	response, err := s.engine.Execute(statement)
	if err != nil {
		return &sqlpb.QueryResponse{Code: sqlpb.StatusCode_EXECUTION_ERROR, ErrorMessage: err.Error()}, nil
	}

	return &sqlpb.QueryResponse{
		Code:    sqlpb.StatusCode_OK,
		Columns: response.Columns,
		Rows:    toProtoRows(response.Rows),
	}, nil
}

func toProtoRows(rows [][]string) []*sqlpb.Row {
	out := make([]*sqlpb.Row, len(rows))
	for i, row := range rows {
		out[i] = &sqlpb.Row{Values: row}
	}
	return out
}
