.PHONY: test proto generate docker-build docker-run

.DEFAULT_GOAL := test

test:
	go test ./...

proto:
	protoc \
		--proto_path=proto/sqlpb \
		--go_out=proto/sqlpb --go_opt=paths=source_relative \
		--go-grpc_out=proto/sqlpb --go-grpc_opt=paths=source_relative \
		sql.proto

generate:
	wire ./internal/wiring

docker-build:
	docker build -t go-sql .

docker-run:
	docker run --rm -p 50051:50051 -v go-sql-data:/data go-sql
