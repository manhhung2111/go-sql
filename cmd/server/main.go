package main

import (
	"flag"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"

	"manhhung2111/go-sql/internal/config"
	"manhhung2111/go-sql/internal/wiring"
	"manhhung2111/go-sql/proto/sqlpb"
)

func main() {
	configPath := flag.String("config", envOr("CONFIG_PATH", "internal/config/config.yml"), "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config from %s: %v", *configPath, err)
	}

	server, err := wiring.InitializeServer(cfg)
	if err != nil {
		log.Fatalf("failed to initialize server: %v", err)
	}

	addr := cfg.Server.Addr()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}

	grpcServer := grpc.NewServer()
	sqlpb.RegisterSqlParserServiceServer(grpcServer, server)

	log.Printf("sql-parser gRPC server listening on %s", addr)
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
