PLUGINS := translator shift-actions move-to-thread

.PHONY: test dist verify

test dist verify:
	@for plugin in $(PLUGINS); do $(MAKE) -C $$plugin $@ || exit 1; done
