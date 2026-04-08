.PHONY: build install test restart

BINARY := shan
INSTALL_PATH := $(shell which $(BINARY) 2>/dev/null || echo /opt/homebrew/bin/$(BINARY))

build:
	go build -o $(INSTALL_PATH) .
	codesign -s - $(INSTALL_PATH) 2>/dev/null || true

install: build

test:
	go test ./...

restart: build
	$(BINARY) daemon stop 2>/dev/null || true
	$(BINARY) daemon start
