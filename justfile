run:
  CGO_ENABLED=1 go run -tags sqlite_fts5 cmd/relay/main.go

build-relay:
  CGO_ENABLED=1 go build -tags sqlite_fts5 -o bin/zooid cmd/relay/main.go

build-import:
  CGO_ENABLED=1 go build -tags sqlite_fts5 -o bin/import cmd/import/main.go

build-export:
  CGO_ENABLED=1 go build -tags sqlite_fts5 -o bin/export cmd/export/main.go

build: build-relay build-import build-export

test:
  CGO_ENABLED=1 go test -tags sqlite_fts5 -v ./...

fmt:
  gofmt -w -s .
