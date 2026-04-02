.PHONY: setup generate build run run-debug clean docker

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

setup: generate
	go mod tidy

generate:
	protoc \
		--go_out=. \
		--go_opt=paths=source_relative \
		internal/nanit/nanitpb/nanit.proto

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/nanit-bridge ./cmd/nanit-bridge

debug:
	go build -o bin/nanit-debug ./cmd/nanit-debug

run: build
	./bin/nanit-bridge

run-debug: debug
	./bin/nanit-debug

clean:
	rm -rf bin/

docker:
	docker build --build-arg VERSION=$(VERSION) -t nanit-bridge .
