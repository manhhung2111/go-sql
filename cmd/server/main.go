package main

import (
	"flag"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"

	"manhhung2111/go-sql/internal/grpcserver"
	"manhhung2111/go-sql/proto/sqlpb"
)

func main() {
	addr := flag.String("addr", envOr("GRPC_ADDR", ":50051"), "address to listen on")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", *addr, err)
	}

	grpcServer := grpc.NewServer()
	sqlpb.RegisterSqlParserServiceServer(grpcServer, grpcserver.New())

	log.Printf("sql-parser gRPC server listening on %s", *addr)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
