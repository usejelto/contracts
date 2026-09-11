.DEFAULT_GOAL := test
.PHONY: build test test-package conformance conformance-twice package signatures
GO ?= go
PYTHON ?= python3
EXE := $(if $(filter Windows_NT,$(OS)),.exe,)
VERSION := $(shell $(PYTHON) -c 'import json; print(json.load(open("spec/contracts/manifest.json"))["version"])')

build:
	mkdir -p bin
	$(GO) build -o bin/runner$(EXE) ./spec/conformance/runner
	$(GO) build -o bin/mockd$(EXE) ./spec/conformance/mockd
	$(GO) build -o bin/refhost$(EXE) ./spec/conformance/refhost
	$(GO) build -o bin/dogfood$(EXE) ./spec/wirecheck/dogfood

test: test-package
	$(GO) test ./... -count=1

test-package:
	$(PYTHON) -m unittest discover -s tests -p 'test_*.py' -v

conformance:
	$(GO) run ./spec/conformance/runner -contracts-version $(VERSION)

conformance-twice:
	$(MAKE) conformance
	$(MAKE) conformance

package:
	$(PYTHON) package.py

# Refresh only the generated read list; ingest/bot/source lists remain curated.
# Reject unexpectedly short responses so an upstream error cannot erase
# historical spam classification.
SPAM_UPSTREAM_REPO ?= https://github.com/matomo-org/referrer-spam-list.git
SPAM_UPSTREAM_URL ?= https://raw.githubusercontent.com/matomo-org/referrer-spam-list/master/spammers.txt
SPAM_READ_FILE := spec/signatures/referrer-spam-read.txt
SPAM_INGEST_FILE := spec/signatures/referrer-spam.txt
SPAM_MIN_HOSTS := 1000

signatures: ## Refresh the generated referrer-spam read table and print every signature pin
	@set -eu; \
	test -f $(SPAM_READ_FILE) || { echo "$(SPAM_READ_FILE) is missing; restore it from git -- its comment header is the template this target rewrites" >&2; exit 1; }; \
	header=$$(awk '/^#/ { print; next } { exit }' $(SPAM_READ_FILE)); \
	case "$$header" in *"# Pinned:"*) ;; *) echo "$(SPAM_READ_FILE): header carries no '# Pinned:' line to rewrite" >&2; exit 1 ;; esac; \
	commit=$$(git ls-remote $(SPAM_UPSTREAM_REPO) HEAD | cut -f1); \
	test -n "$$commit" || { echo "cannot resolve HEAD of $(SPAM_UPSTREAM_REPO)" >&2; exit 1; }; \
	body=$$(mktemp); \
	curl -fsS $(SPAM_UPSTREAM_URL) | tr -d '\r' | tr 'A-Z' 'a-z' | sed -e 's/[[:space:]]//g' -e '/^$$/d' -e '/^#/d' | sort -u > $$body; \
	count=$$(grep -c . $$body || true); \
	test "$$count" -ge $(SPAM_MIN_HOSTS) || { rm -f $$body; echo "refusing to rewrite $(SPAM_READ_FILE): upstream returned $$count hosts, under the $(SPAM_MIN_HOSTS) floor" >&2; exit 1; }; \
	before=$$(grep -cvE '^#|^$$' $(SPAM_READ_FILE) || true); \
	printf '%s\n' "$$header" | sed "s|^# Pinned:.*|# Pinned:   $$commit · fetched $$(date -u +%Y-%m-%d) · $$count hostnames|" > $(SPAM_READ_FILE).new; \
	cat $$body >> $(SPAM_READ_FILE).new; \
	mv $(SPAM_READ_FILE).new $(SPAM_READ_FILE); \
	ingest=$$(mktemp); read_only=$$(mktemp); \
	grep -vE '^#|^$$' $(SPAM_INGEST_FILE) | sort -u > $$ingest; \
	sort -u $$body > $$read_only; \
	missing=$$(comm -23 $$ingest $$read_only | tr '\n' ' '); \
	rm -f $$body $$ingest $$read_only; \
	echo "$(SPAM_READ_FILE): $$before -> $$count hostnames, upstream $$commit"; \
	if [ -n "$$missing" ]; then echo "ingest-only hosts (unioned into the read table at load, spec/metrics.md §6b): $$missing"; fi; \
	echo; \
	echo "signature pins -- a hand-curated table is never regenerated, so read it before you trust it:"; \
	grep -H -E '^# (Upstream|Pinned|Refresh):' spec/signatures/*.yaml spec/signatures/*.txt
