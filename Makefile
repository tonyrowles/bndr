.PHONY: build run test race release clean

VERSION ?= dev

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/bndr .

run:
	go run .

test:
	go test ./...

race:
	go test -race ./...

release:
	./scripts/release.sh "$(VERSION)"

clean:
	rm -f bin/bndr
