# ship:begin (owned by ship init)
# The gate: everything CI checks, configured in ship.yaml. Master's ship, as the pre-push hook and the @master
# workflows run it: its sha asked of GitHub (a proxy lags on @master). A local ship: SHIP_CMD=<go-installed binary>,
# one name for here and the hook, so make check and the push gate never run different ships.
SHIP_CMD ?= sha=$$(git ls-remote https://github.com/trick77/ship refs/heads/master | cut -f1); \
	[ -n "$$sha" ] || { echo "cannot resolve ship's master on GitHub (git ls-remote, error above)"; exit 1; }; \
	go run github.com/trick77/ship@$$sha

.PHONY: check hooks lint

check:
	$(SHIP_CMD) check

hooks:
	$(SHIP_CMD) hooks

# Quick lint while editing, at the version check runs.
lint:
	$(SHIP_CMD) lint
# ship:end
