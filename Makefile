PREFIX ?= /usr/local
GOWORK := off
export GOWORK

.PHONY: build test check install

build:
	go build -trimpath -o bin/looper .

test:
	go test -race -count=1 ./...

check: test
	go vet ./...
	test -z "$$(gofmt -l *.go)"

install:
	test -x bin/looper
	install -d "$(DESTDIR)$(PREFIX)/bin"
	install -m 755 bin/looper "$(DESTDIR)$(PREFIX)/bin/looper"
