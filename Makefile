PLUGINS := translator shift-actions

.PHONY: test dist verify

test dist verify:
	@for plugin in $(PLUGINS); do $(MAKE) -C $$plugin $@ || exit 1; done
