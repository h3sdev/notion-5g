# Build estático (sin CGO: modernc.org/sqlite es SQLite puro en Go, no necesita gcc
# ni libsqlite3 en el contenedor final).
FROM golang:1.25-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 notion5g
COPY --from=build /out/server /usr/local/bin/server
RUN mkdir -p /data && chown notion5g:notion5g /data
USER notion5g
VOLUME ["/data"]
ENV DB_PATH=/data/notion5g.db
ENV LISTEN_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
