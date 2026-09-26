package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("sql-parser gRPC server listening on %s", addr)
		serveErr <- grpcServer.Serve(lis)
	}()

	select {
	case err := <-serveErr:
		log.Fatalf("server stopped unexpectedly: %v", err)
	case <-ctx.Done():
		log.Println("shutdown signal received, stopping gracefully...")
		grpcServer.GracefulStop()
		log.Println("server stopped")
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
