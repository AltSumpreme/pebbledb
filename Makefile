GO ?= go

.PHONY: build test race vet benchmark-smoke verify

build:
	$(GO) build ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

benchmark-smoke:
	$(GO) test -run '^$$' -bench . -benchtime=1x ./storage/lsm ./sql/engine

verify: build test race vet benchmark-smoke
