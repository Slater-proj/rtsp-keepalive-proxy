.PHONY: build test lint run clean docker docker-push

APP_NAME := rtsp-keepalive-proxy
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -s -w -X main.version=$(VERSION)

## build: Compile the binary
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(APP_NAME) ./cmd/proxy

## test: Run unit tests with race detector
test:
	go test -v -race -count=1 ./...

## coverage: Generate test coverage report
coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## lint: Run go vet
lint:
	go vet ./...

## run: Build and run locally
run: build
	./$(APP_NAME) -config config.yaml

## clean: Remove build artifacts
clean:
	rm -f $(APP_NAME) coverage.out coverage.html

## docker: Build Docker image
docker:
	docker build -t $(APP_NAME):$(VERSION) -t $(APP_NAME):latest .

## docker-push: Push Docker image (set REGISTRY env var)
docker-push:
	docker tag $(APP_NAME):latest $(REGISTRY)/$(APP_NAME):$(VERSION)
	docker tag $(APP_NAME):latest $(REGISTRY)/$(APP_NAME):latest
	docker push $(REGISTRY)/$(APP_NAME):$(VERSION)
	docker push $(REGISTRY)/$(APP_NAME):latest

## help: Show this help
help:
	@grep -E '^## ' Makefile | sed 's/## //' | column -t -s ':'
