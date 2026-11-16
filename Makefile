.PHONY: build test run clean

build:
	go build -o bin/findr ./cmd/findr

test:
	go test -v ./...

run: build
	./bin/findr

clean:
	rm -rf bin/ findr.db
