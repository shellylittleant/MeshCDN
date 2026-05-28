.PHONY: build clean source-snapshot release vet fmt

BINARY := cdn-agent
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "untracked")
BUILDTIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X github.com/example/meshcdn/internal/version.CommitHash=$(COMMIT) \
           -X github.com/example/meshcdn/internal/version.BuildTime=$(BUILDTIME) \
           -s -w

EMBED_DIR := internal/version/source

build: source-snapshot
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/cdn-agent

# source-snapshot: copy all source into the embed location.
# Note: we copy go.mod / go.sum as .txt files because Go would otherwise
# treat the embed dir as a separate module.
source-snapshot:
	@mkdir -p $(EMBED_DIR)
	@rm -rf $(EMBED_DIR)/cmd $(EMBED_DIR)/docs $(EMBED_DIR)/scripts $(EMBED_DIR)/internal-snapshot
	@cp -r cmd $(EMBED_DIR)/
	@cp -r docs $(EMBED_DIR)/
	@cp -r scripts $(EMBED_DIR)/
	@mkdir -p $(EMBED_DIR)/internal-snapshot
	@find internal -maxdepth 1 -mindepth 1 -type d ! -name version | xargs -I{} cp -r {} $(EMBED_DIR)/internal-snapshot/
	@mkdir -p $(EMBED_DIR)/internal-snapshot/version
	@cp internal/version/version.go $(EMBED_DIR)/internal-snapshot/version/
	@cp go.mod $(EMBED_DIR)/go.mod.txt 2>/dev/null || true
	@cp go.sum $(EMBED_DIR)/go.sum.txt 2>/dev/null || true
	@cp Makefile $(EMBED_DIR)/Makefile.txt 2>/dev/null || true
	@echo "$(COMMIT)" > $(EMBED_DIR)/COMMIT
	@echo "$(BUILDTIME)" > $(EMBED_DIR)/BUILDTIME

clean:
	rm -f $(BINARY)
	rm -rf $(EMBED_DIR)
	mkdir -p $(EMBED_DIR)
	echo "placeholder" > $(EMBED_DIR)/PLACEHOLDER

vet:
	go vet ./...

fmt:
	gofmt -w .

release: build
	@VER=$$(./$(BINARY) --version 2>/dev/null | awk '{print $$2}'); \
	TARBALL=meshcdn-$$VER-linux-amd64.tar.gz; \
	mkdir -p dist/source; \
	cp $(BINARY) dist/; \
	cp scripts/install.sh dist/; \
	cp scripts/bootstrap.sh dist/; \
	cp -r $(EMBED_DIR)/* dist/source/; \
	echo "$(COMMIT) $(BUILDTIME)" > dist/VERSION; \
	tar czf $$TARBALL -C dist .; \
	rm -rf dist; \
	echo "Released: $$TARBALL"
