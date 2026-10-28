.PHONY: build test vet fmt run clean

build:
	go build -trimpath -ldflags="-s -w" -o bin/sysgate ./cmd/sysgate

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

run:
	go run ./cmd/sysgate

clean:
	rm -rf bin dist
