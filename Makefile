.PHONY: setup generate build run clean

setup: generate
	go mod tidy

generate:
	protoc \
		--go_out=. \
		--go_opt=paths=source_relative \
		internal/nanit/nanitpb/nanit.proto

build:
	go build -o bin/nanit-bridge ./cmd/nanit-bridge

debug:
	go build -o bin/nanit-debug ./cmd/nanit-debug

run: build
	./bin/nanit-bridge

clean:
	rm -rf bin/

docker:
	docker build -t nanit-bridge .
