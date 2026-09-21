package grpcserver

import (
	"context"

	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/parser"
	"manhhung2111/go-sql/proto/sqlpb"
)

type Server struct {
	sqlpb.UnimplementedSqlParserServiceServer
	config *config.Config
}

func NewServer(cfg *config.Config) *Server {
	return &Server{config: cfg}
}

func (s *Server) ParseQuery(_ context.Context, req *sqlpb.QueryRequest) (*sqlpb.QueryResponse, error) {
	tokens := parser.NewLexer(req.GetSql()).Lexing()
	_, err := parser.NewParser(tokens).Parse()
	if err != nil {
		return &sqlpb.QueryResponse{Code: sqlpb.StatusCode_PARSE_ERROR, ErrorMessage: err.Error()}, nil
	}

	return &sqlpb.QueryResponse{Code: sqlpb.StatusCode_OK}, nil
}
