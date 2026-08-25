# gaze — a single-binary system monitor for Linux terminals.
#
# The program only runs on Linux: it reads /proc and /sys directly. It still
# compiles on macOS, and the tests run there against the fixtures in
# internal/metrics/testdata, so the edit-test loop stays on the dev machine and
# only `make run` needs a Linux kernel.

BIN     := gaze
PKG     := github.com/hammondus/gaze
DIST    := dist
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# -s -w drop the symbol table and DWARF data, which halves the binary and
# costs nothing but stack traces from a crashed release build.
LDFLAGS := -s -w -X main.version=$(VERSION)

# CGO is off so os/user resolves names by parsing /etc/passwd in pure Go. With
# CGO on, the binary links against the host's libc and stops being portable
# across distributions, which defeats the point of shipping one file.
GOENV := CGO_ENABLED=0

# The container image used by `make run`. Only needed to hold a filesystem
# around the binary; the binary itself is static.
RUNIMG := alpine:3.20

.PHONY: build
build: ## Compile for the dev machine. Checks it builds; it will not run here.
	$(GOENV) go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/gaze

.PHONY: test
test: ## Vet and run the tests, including the linux/arm64 build.
	go vet ./...
	go test ./...
	GOOS=linux GOARCH=arm64 go vet ./...

.PHONY: frame
frame: ## Print one rendered frame from synthetic data, to check the layout.
	go test ./internal/ui -run TestRenderDemo -v

.PHONY: run
run: $(DIST)/$(BIN)-linux-arm64 ## Run the real binary against a Linux kernel.
	docker run --rm -it \
	  --pid=host \
	  -v $(PWD)/$(DIST):/dist:ro \
	  $(RUNIMG) /dist/$(BIN)-linux-arm64

.PHONY: release
release: $(DIST)/$(BIN)-linux-arm64 $(DIST)/$(BIN)-linux-amd64 $(DIST)/SHA256SUMS ## Build the deploy artifacts.
	@ls -lh $(DIST)

# dist is also the name of the output directory, so without .PHONY make finds
# the directory, calls the target up to date, and reports success having built
# nothing. Copying a stale binary to a server is the failure that follows.
.PHONY: dist
dist: release ## Build the deploy artifacts. Alias for release.

# Checksums let a target machine confirm it got the bytes you built. The tool
# is named sha256sum on Linux and shasum on macOS.
$(DIST)/SHA256SUMS: $(DIST)/$(BIN)-linux-arm64 $(DIST)/$(BIN)-linux-amd64
	@cd $(DIST) && { command -v sha256sum >/dev/null && sha256sum $(BIN)-linux-* \
	  || shasum -a 256 $(BIN)-linux-*; } > SHA256SUMS
	@cat $@

.PHONY: publish
publish: release ## Attach the artifacts to a GitHub release for the current tag.
	@test -n "$$(git tag --points-at HEAD)" || \
	  { echo "publish: HEAD carries no tag. Run: git tag -a vX.Y.Z -m ... && git push --tags"; exit 1; }
	@test -z "$$(git status --porcelain)" || \
	  { echo "publish: the working tree is dirty; commit first"; exit 1; }
	gh release create "$$(git tag --points-at HEAD | head -1)" \
	  $(DIST)/$(BIN)-linux-arm64 $(DIST)/$(BIN)-linux-amd64 $(DIST)/SHA256SUMS \
	  --generate-notes

# Only Linux targets: there is nothing to ship for macOS or Windows, because
# the metrics come from /proc.
$(DIST)/$(BIN)-linux-%: $(shell find . -name '*.go' -not -name '*_test.go')
	@mkdir -p $(DIST)
	$(GOENV) GOOS=linux GOARCH=$* go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/gaze

.PHONY: install
install: ## Install into GOBIN on this machine.
	$(GOENV) go install -ldflags "$(LDFLAGS)" ./cmd/gaze

.PHONY: clean
clean: ## Remove build output.
	rm -rf $(BIN) $(DIST)

.PHONY: help
help: ## List the targets.
	@grep -hE '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*## /\t/' | expand -t 12
