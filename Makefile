ARGS ?=

run:
	go run ./cmd/hexlet-go-crawler "$(URL)"
build:
	go build -o bin/hexlet-go-crawler ./cmd/hexlet-go-crawler
lint:
	golangci-lint run
lint-fix:
	golangci-lint run --fix
test:
	go test -v ./... $(ARGS)
test-coverage:
	go test -coverprofile=coverage.out ./...