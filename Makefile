.PHONY: test proto

.DEFAULT_GOAL := test

test:
	go test ./...

proto:
	protoc \
		--proto_path=proto/sqlpb \
		--go_out=proto/sqlpb --go_opt=paths=source_relative \
		--go-grpc_out=proto/sqlpb --go-grpc_opt=paths=source_relative \
		sql.proto
