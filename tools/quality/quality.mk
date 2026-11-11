# Shared quality and security targets for the Jelto repositories.
#
# This file is maintained in the contracts repository under tools/quality/ and
# arrives in every other repository as a checksummed snapshot. Change it there
# and refresh the snapshots; an edited copy fails the snapshot check.
#
# Every program it runs is the exact version tools/quality/tools.json pins,
# installed on first use into a per-user cache by install_tools.py after its
# SHA-256 matched. None of them is a project dependency, and nothing falls back
# to whatever version happens to be on PATH, so a local run and CI agree.
#
# A consumer may set these before including this file:
#   QUALITY_COMMUNITY_RULES  opengrep community rule directories for its
#                            languages, for example `go python`
#   GITLEAKS_BASELINE        a reviewed gitleaks report of historical findings;
#                            only findings outside it fail
#   SAST_EXCLUDE             extra paths opengrep skips (vendored or generated
#                            code that another repository scans)

QUALITY_DIR := $(patsubst %/,%,$(dir $(lastword $(MAKEFILE_LIST))))
QUALITY_PYTHON ?= python3
QUALITY_TOOL = $(QUALITY_PYTHON) $(QUALITY_DIR)/install_tools.py
QUALITY_COMMUNITY_RULES ?=
GITLEAKS_BASELINE ?=
SAST_EXCLUDE ?=

.PHONY: hooks tools secrets osv workflow-audit sast

hooks: ## Run the commit-message and staged-secret checks before every commit
	git config core.hooksPath .githooks
	@echo 'hooks: core.hooksPath is .githooks'

tools: ## Install every pinned quality tool into the per-user cache
	$(QUALITY_TOOL) install

# The full history, not only the working tree: a secret removed in a later
# commit is still published by every clone.
# A shallow clone would scan only its last commit and pass, so it is refused.
secrets: ## Scan the whole Git history for secrets
	@if [ "$$(git rev-parse --is-shallow-repository)" = true ]; then \
		echo 'secrets: this clone is shallow, so its history cannot be scanned; fetch all of it (actions/checkout: fetch-depth: 0)' >&2; \
		exit 1; fi
	$(QUALITY_TOOL) run gitleaks git --redact --no-banner \
		$(if $(GITLEAKS_BASELINE),--baseline-path $(GITLEAKS_BASELINE),) .

# The vulnerability database is downloaded per ecosystem and matched locally,
# so the dependency list never leaves this machine. Every advisory against a
# locked version is reported, reached or not: reachability is govulncheck's
# job in a Go repository, and Go call analysis cannot load a module whose
# requirements resolve only through a generated module file. An ignored
# advisory lives in the repository's one osv-scanner.toml, with its reason and
# an expiry date, and applies to every lockfile in the repository.
osv: ## Check every lockfile against the OSV vulnerability database
	$(QUALITY_TOOL) run osv-scanner scan source --recursive --no-call-analysis=go \
		$(if $(wildcard osv-scanner.toml),--config osv-scanner.toml,) \
		--offline-vulnerabilities --download-offline-databases .

# zizmor runs its offline audits only, so a local run and CI see the same
# findings without handing a token to the commands that run beside it.
workflow-audit: ## Lint and audit the GitHub Actions configuration
	$(QUALITY_TOOL) run actionlint
	$(QUALITY_TOOL) run zizmor --offline --config $(QUALITY_DIR)/zizmor.yml .

# Community rules first, then the repository's own rules in .opengrep/, each of
# which keeps its test cases beside it. An exception is a `nosemgrep: <rule-id>`
# comment on the one line it excuses, followed by the reason.
sast: ## Run the pinned community rules for this repository's languages and its own rules
	@set -e; rules=$$($(QUALITY_TOOL) path opengrep-rules); set --; \
	for dir in $(QUALITY_COMMUNITY_RULES); do \
		test -d "$$rules/$$dir" || { echo "sast: no community rule directory $$dir" >&2; exit 1; }; \
		set -- "$$@" --config "$$rules/$$dir"; \
	done; \
	if [ -d .opengrep ]; then set -- "$$@" --config .opengrep; fi; \
	if [ $$# -eq 0 ]; then echo 'sast: no rules configured' >&2; exit 1; fi; \
	for path in $(SAST_EXCLUDE); do set -- "$$@" --exclude "$$path"; done; \
	echo "opengrep scan $$*"; \
	$(QUALITY_TOOL) run opengrep scan --error --quiet --disable-version-check \
		--exclude 'vendor' --exclude 'node_modules' --exclude '.opengrep' "$$@" .
	@if [ -d .opengrep ]; then $(QUALITY_TOOL) run opengrep test .opengrep; fi

