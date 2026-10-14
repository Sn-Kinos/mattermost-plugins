PLUGIN_ID := io.github.sn-kinos.translator
VERSION := 0.1.0
DIST_DIR := dist
BUNDLE_DIR := $(DIST_DIR)/$(PLUGIN_ID)
ARCHIVE := $(DIST_DIR)/$(PLUGIN_ID)-$(VERSION).tar.gz
GOFLAGS := -trimpath -buildvcs=false -ldflags='-buildid='

.PHONY: test dist clean verify

test:
	go test ./...
	npm --prefix webapp test

dist: clean
	mkdir -p $(BUNDLE_DIR)/server/dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -o server/dist/plugin-linux-amd64 ./server
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -o server/dist/plugin-linux-arm64 ./server
	cp server/dist/plugin-linux-amd64 $(BUNDLE_DIR)/server/dist/plugin-linux-amd64
	cp server/dist/plugin-linux-arm64 $(BUNDLE_DIR)/server/dist/plugin-linux-arm64
	cp plugin.json $(BUNDLE_DIR)/plugin.json
	mkdir -p $(BUNDLE_DIR)/webapp
	cp webapp/plugin.js $(BUNDLE_DIR)/webapp/plugin.js
	go run ./build/package -root $(BUNDLE_DIR) -out $(ARCHIVE)

clean:
	rm -rf $(DIST_DIR) server/dist

verify:
	npm --prefix webapp ci --no-audit --no-fund
	npm --prefix webapp test
	test -z "$$(gofmt -l server)"
	go vet ./...
	go test -race ./server/... -count=1
	go test ./... -count=1
	$(MAKE) dist
	@set -eu; \
		command -v rg >/dev/null 2>&1 || { echo "rg is required for release verification" >&2; exit 1; }; \
		placeholder_status=0; \
		rg -n 'test[.]skip|test[.]only|TO[D]O|FIX[M]E|not[ ]implemented' server || placeholder_status=$$?; \
		case $$placeholder_status in \
			0) echo "incomplete implementation or skipped test found under server" >&2; exit 1 ;; \
			1) ;; \
			*) echo "server placeholder scan failed with status $$placeholder_status" >&2; exit "$$placeholder_status" ;; \
		esac; \
		skip_status=0; \
		rg -n --glob '*_test.go' '[.](Skip|Skipf|SkipNow)[[:space:]]*[(]' server || skip_status=$$?; \
		case $$skip_status in \
			0) echo "skipped Go test found under server" >&2; exit 1 ;; \
			1) ;; \
			*) echo "server Go skip scan failed with status $$skip_status" >&2; exit "$$skip_status" ;; \
		esac
	@set -eu; \
		archive_count=0; \
		for archive in $(DIST_DIR)/*.tar.gz; do \
			test -f "$$archive" || continue; \
			archive_count=$$((archive_count + 1)); \
			test "$$archive" = "$(ARCHIVE)" || { echo "unexpected plugin archive: $$archive" >&2; exit 1; }; \
		done; \
		test "$$archive_count" = "1" || { echo "expected one plugin archive, found $$archive_count" >&2; exit 1; }; \
		entries=$$(tar -tzf "$(ARCHIVE)"); \
		expected=$$(printf '%s\n' 'plugin.json' 'server/dist/plugin-linux-amd64' 'server/dist/plugin-linux-arm64' 'webapp/plugin.js'); \
		test "$$entries" = "$$expected" || { \
			echo "archive entries differ from the plugin allowlist" >&2; \
			printf '%s\n' "$$entries" >&2; \
			exit 1; \
		}; \
		printf '%s\n' "$$entries"
	@set -eu; \
		command -v shasum >/dev/null 2>&1 || { echo "shasum is required for release verification" >&2; exit 1; }; \
		$(MAKE) clean dist >/dev/null; \
		first_hash_line=$$(shasum -a 256 "$(ARCHIVE)"); \
		first_sha=$${first_hash_line%% *}; \
		test -n "$$first_sha"; \
		$(MAKE) clean dist >/dev/null; \
		second_hash_line=$$(shasum -a 256 "$(ARCHIVE)"); \
		second_sha=$${second_hash_line%% *}; \
		test -n "$$first_sha" && test "$$first_sha" = "$$second_sha" || { \
			echo "non-deterministic archive: $$first_sha != $$second_sha" >&2; \
			exit 1; \
		}; \
		echo "deterministic archive sha256=$$second_sha"
