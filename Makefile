BINARY_NAME=bin/mqtt

.PHONY: build clean interop

# Requires Mosquitto and Paho; see docs/interoperability.md for installation.
interop:
	go test -race -tags interop ./mqtt -run TestInterop -v -count=1 -timeout 180s

build:
	mkdir -p bin
	GOARCH=arm64 GOOS=darwin go build -ldflags="-s -w" -o ${BINARY_NAME}-darwin-arm64 ./cmd/mqtt
	GOARCH=amd64 GOOS=darwin go build -ldflags="-s -w" -o ${BINARY_NAME}-darwin-amd64 ./cmd/mqtt
	GOARCH=amd64 GOOS=linux go build -ldflags="-s -w" -o ${BINARY_NAME}-linux-amd64 ./cmd/mqtt
	GOARCH=amd64 GOOS=windows go build -ldflags="-s -w" -o ${BINARY_NAME}-windows-amd64.exe ./cmd/mqtt

clean:
	rm -f ${BINARY_NAME}-darwin-arm64
	rm -f ${BINARY_NAME}-darwin-amd64
	rm -f ${BINARY_NAME}-linux-amd64
	rm -f ${BINARY_NAME}-windows-amd64.exe
