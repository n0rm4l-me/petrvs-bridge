.PHONY: build vet test test-emulator fuzz cover vuln clean

build:
	go build -o bin/ ./cmd/...

vet:
	go vet ./...

# Unit tests and the in-memory end-to-end tests. Tests that need the emulator are skipped.
test:
	go test -race -count=1 ./...

# Everything, including the tests on the Pub/Sub emulator and the real-binary smoke test.
test-emulator:
	scripts/with-emulator.sh go test -race -count=1 ./...

fuzz:
	go test -run xxx -fuzz FuzzDecrypt -fuzztime 30s ./internal/ratchet/

cover:
	scripts/with-emulator.sh go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

clean:
	rm -rf bin coverage.out
