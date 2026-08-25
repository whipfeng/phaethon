BINARY := phaethon
SRC := .
DIST_DIR := dist

# Detect git tag for version (fallback to "dev")
GIT_TAG := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-s -w -X main.Version=$(GIT_TAG)"

.PHONY: all clean linux linux-arm64 windows windows7 windows-arm64 darwin-amd64 darwin-arm64 version

all: linux linux-arm64 windows windows7 windows-arm64 darwin-amd64 darwin-arm64

# --- Linux ---

linux:
	@echo "=== Building Linux amd64 ($(GIT_TAG)) ==="
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/linux-amd64/$(BINARY) $(SRC)

linux-arm64:
	@echo "=== Building Linux arm64 ($(GIT_TAG)) ==="
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(DIST_DIR)/linux-arm64/$(BINARY) $(SRC)

# --- Windows ---

windows:
	@echo "=== Building Windows amd64 ($(GIT_TAG)) ==="
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/windows-amd64/$(BINARY).exe $(SRC)

windows7:
	@echo "=== Building Windows 7 amd64 ($(GIT_TAG)), legacy compiler ==="
	@if [ -z "$(GO_LEGACY_WIN7)" ]; then \
		echo "Error: GO_LEGACY_WIN7 is not set; point it to the go-legacy-win7 compiler, e.g. C:\\\\go-legacy-win7\\\\bin\\\\go.exe"; \
		exit 1; \
	fi
	$(GO_LEGACY_WIN7) build $(LDFLAGS) -o $(DIST_DIR)/windows7-amd64/$(BINARY).exe $(SRC)

windows-arm64:
	@echo "=== Building Windows arm64 ($(GIT_TAG)) ==="
	GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(DIST_DIR)/windows-arm64/$(BINARY).exe $(SRC)

# --- macOS ---

darwin-amd64:
	@echo "=== Building macOS amd64 ($(GIT_TAG)) ==="
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/darwin-amd64/$(BINARY)-amd64 $(SRC)

darwin-arm64:
	@echo "=== Building macOS arm64 ($(GIT_TAG)) ==="
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(DIST_DIR)/darwin-arm64/$(BINARY)-arm64 $(SRC)

# --- Utils ---

version:
	@echo "Version: $(GIT_TAG)"

clean:
	@echo "Cleaning dist directory..."
	rm -rf $(DIST_DIR)

# Ensure output directories exist before build
$(DIST_DIR)/linux-amd64 $(DIST_DIR)/linux-arm64 $(DIST_DIR)/windows-amd64 $(DIST_DIR)/windows7-amd64 $(DIST_DIR)/windows-arm64 $(DIST_DIR)/darwin-amd64 $(DIST_DIR)/darwin-arm64:
	mkdir -p $@
