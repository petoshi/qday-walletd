QDAY_GO := $(if $(wildcard ../qday/.tools/go/bin/go),../qday/.tools/go/bin/go,go)

.PHONY: build test

build:
	mkdir -p build
	$(QDAY_GO) build -trimpath -o build/qday-walletd .

test:
	$(QDAY_GO) test ./...
	$(QDAY_GO) vet ./...
