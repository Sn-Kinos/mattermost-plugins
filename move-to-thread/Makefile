PLUGIN_ID := io.github.sn-kinos.move-to-thread
VERSION := 0.1.0
ARCHIVE := dist/$(PLUGIN_ID)-$(VERSION).tar.gz
GOFLAGS := -trimpath -buildvcs=false -ldflags='-buildid='

.PHONY: test dist verify

test:
	go test ./...
	npm --prefix webapp test

dist:
	rm -rf dist server/dist
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o server/dist/plugin-linux-amd64 ./server
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o server/dist/plugin-linux-arm64 ./server
	COPYFILE_DISABLE=1 tar -czf $(ARCHIVE) plugin.json server/dist/plugin-linux-amd64 server/dist/plugin-linux-arm64 webapp/plugin.js

verify:
	npm --prefix webapp ci --no-audit --no-fund
	test -z "$$(gofmt -l server)"
	go vet ./...
	go test -race -count=1 ./...
	npm --prefix webapp test
	$(MAKE) dist
