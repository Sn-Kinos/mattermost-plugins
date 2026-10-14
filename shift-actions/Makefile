PLUGIN_ID := io.github.sn-kinos.shift-actions
VERSION := 0.1.0
ARCHIVE := dist/$(PLUGIN_ID)-$(VERSION).tar.gz

.PHONY: test dist verify

test:
	npm --prefix webapp test

dist:
	mkdir -p dist
	COPYFILE_DISABLE=1 tar -czf $(ARCHIVE) plugin.json webapp/plugin.js

verify:
	npm --prefix webapp ci --no-audit --no-fund
	$(MAKE) test dist
