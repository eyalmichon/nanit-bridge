FROM golang:1.25-bookworm AS builder

ARG VERSION=dev

RUN apt-get update && apt-get install -y protobuf-compiler && rm -rf /var/lib/apt/lists/*
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN make generate
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /nanit-bridge ./cmd/nanit-bridge

FROM gcr.io/distroless/static-debian12

COPY --from=builder /nanit-bridge /nanit-bridge

EXPOSE 1935 8080

ENTRYPOINT ["/nanit-bridge"]
