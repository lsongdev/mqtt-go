BINARY_NAME=bin/mqtt

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
