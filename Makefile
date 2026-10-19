BIN := bin/ghfleet

.PHONY: build test race vet fmt install clean

build:
	go build -o $(BIN) ./cmd/ghfleet

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

install:
	go install ./cmd/ghfleet

clean:
	rm -rf bin
