.PHONY: build test vet lint
build:
	go build -o bin/wtg ./cmd/wtg
test:
	go test -race ./...
vet:
	go vet ./...
	GOOS=windows go vet ./...
lint: vet
	test -z "$$(gofmt -l .)"
