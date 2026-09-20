GO ?= go
BIN := bin/wifi-watchdog
PREFIX ?= /usr/local
GOARCH_TARGET ?= arm64

.PHONY: all build test vet fmt check install uninstall clean build-arm64

all: build test vet

build:
	mkdir -p bin
	$(GO) build -trimpath -o $(BIN) ./cmd/wifi-watchdog

# Suffixed output so a host-arch binary can never be copied to a device of
# another architecture by accident.
build-arm64:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH_TARGET) $(GO) build -trimpath \
		-o $(BIN)-linux-$(GOARCH_TARGET) ./cmd/wifi-watchdog

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

# fmt must leave nothing to do, and vet and the tests must pass.
check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	$(GO) vet ./...
	$(GO) test ./...

install: build
	install -D -m 0755 $(BIN) $(DESTDIR)$(PREFIX)/bin/wifi-watchdog
	install -D -m 0644 systemd/wifi-watchdog.service $(DESTDIR)/etc/systemd/system/wifi-watchdog.service
	@echo "Now: systemctl daemon-reload && systemctl enable --now wifi-watchdog"

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/wifi-watchdog
	rm -f $(DESTDIR)/etc/systemd/system/wifi-watchdog.service

clean:
	rm -rf bin/
