# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26.4

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Modules first, so this layer is cached until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# The data directory must exist and belong to the runtime user before it
# becomes a volume, or a fresh volume would not be writable.
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
COPY --from=build --chown=nonroot:nonroot /data /data
COPY deploy/config.docker.yml /etc/go-sql/config.yml

ENV CONFIG_PATH=/etc/go-sql/config.yml
EXPOSE 50051
VOLUME /data

# Exec form: the server is PID 1 and receives SIGTERM directly, so it shuts
# down gracefully and closes its files.
ENTRYPOINT ["/server"]
