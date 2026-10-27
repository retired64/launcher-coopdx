.PHONY: build test run fmt vet clean

build:
	go build -trimpath -o coop-loader ./cmd/coop-loader

run:
	go run ./cmd/coop-loader

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -f coop-loader coop-loader.exe
