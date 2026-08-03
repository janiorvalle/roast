GORELEASER_VERSION ?= v2.11.2
GORELEASER = go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

.PHONY: build vet test release-check snapshot install-smoke verify install-hooks

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

release-check:
	$(GORELEASER) check

snapshot:
	$(GORELEASER) release --snapshot --clean

install-smoke: snapshot
	./scripts/install-smoke.sh

verify: build vet test release-check install-smoke

install-hooks:
	@command -v gitleaks >/dev/null || { echo "gitleaks is required: https://github.com/gitleaks/gitleaks"; exit 1; }
	git config core.hooksPath .githooks
	@echo "Installed the gitleaks pre-commit hook."
