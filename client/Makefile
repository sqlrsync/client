# SQLite Rsync Go Client Makefile
.PHONY: all build clean test deps check-deps install-deps install run help

# Build configuration
BINARY_NAME := sqlrsync
BUILD_DIR := ./bin
MAIN_FILE := .

# Go build flags
CGO_ENABLED := 1
GOFLAGS := -v
LDFLAGS := -w -s

# Bridge configuration
BRIDGE_LIB_DIR := ../bridge
SQLITE_RSYNC_LIB = $(BRIDGE_LIB_DIR)/libsqlite_rsync.a

$(SQLITE_RSYNC_LIB):
	$(MAKE) -C ../bridge -f Makefile



SQLITE_INCLUDE := ../sqlite/install/include
SQLITE_LIB := ../sqlite/install/lib

# Default target
all: check-deps $(SQLITE_RSYNC_OBJ) build


# Check if required system dependencies are available
check-deps:
	@echo "Checking system dependencies..."
	@which sqlite3 > /dev/null || (echo "ERROR: sqlite3 not found. Install with: apt-get install sqlite3 libsqlite3-dev" && exit 1)
	@pkg-config --exists sqlite3 || (echo "ERROR: sqlite3 development headers not found. Install with: apt-get install libsqlite3-dev" && exit 1)
	@echo "✓ System dependencies OK"

# Install system dependencies (Ubuntu/Debian)
install-deps:
	@echo "Installing system dependencies..."
	sudo apt-get update
	sudo apt-get install -y sqlite3 libsqlite3-dev build-essential
	@echo "✓ System dependencies installed"

# Download Go dependencies
deps:
	@echo "Downloading Go dependencies..."
	go mod download
	go mod tidy
	@echo "✓ Go dependencies ready"

# Build the binary
build: $(SQLITE_RSYNC_LIB)
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) CGO_LDFLAGS="-L$(BRIDGE_LIB_DIR) -lsqlite_rsync" go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_FILE)
	@echo "✓ Build complete: $(BUILD_DIR)/$(BINARY_NAME)"

# Install the binary to system path
install: build
	@echo "Installing $(BINARY_NAME) to /usr/local/bin/..."
	cp $(BUILD_DIR)/$(BINARY_NAME) /usr/local/bin/$(BINARY_NAME)
	@echo "✓ Install complete: /usr/local/bin/$(BINARY_NAME)"

# Build with debug symbols
build-debug: check-deps
	@echo "Building $(BINARY_NAME) with debug symbols..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -gcflags="all=-N -l" -o $(BUILD_DIR)/$(BINARY_NAME)-debug $(MAIN_FILE)
	@echo "✓ Debug build complete: $(BUILD_DIR)/$(BINARY_NAME)-debug"

# Run tests
test: check-deps
	@echo "Running tests..."
	CGO_ENABLED=$(CGO_ENABLED) go test -v ./...
	@echo "✓ Tests complete"

# Run the program with default database
run: build
	@echo "Running $(BINARY_NAME)..."
	$(BUILD_DIR)/$(BINARY_NAME) --database ./database.db --url ws://nuc3:8787 --verbose

# Run in dry-run mode
run-dry: build
	@echo "Running $(BINARY_NAME) in dry-run mode..."
	$(BUILD_DIR)/$(BINARY_NAME) --database ./database.db --url ws://nuc3:8787 --verbose --dry-run

# Create example database
create-db:
	@echo "Creating example database..."
	@sqlite3 database.db <<< "CREATE TABLE IF NOT EXISTS test (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO test (name) VALUES ('example1'), ('example2'), ('example3');"
	@echo "✓ Example database created: database.db"

# Check database info
db-info:
	@echo "Database information:"
	@sqlite3 database.db "PRAGMA page_size; PRAGMA page_count; PRAGMA journal_mode; SELECT COUNT(*) as page_count FROM sqlite_dbpage('main');"

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR)
	@rm -f $(C_OBJECT)
	@echo "✓ Clean complete"

# Show help
help:
	@echo "SQLite Rsync Go Client Build System"
	@echo ""
	@echo "Available targets:"
	@echo "  all          - Check dependencies and build (default)"
	@echo "  build        - Build the binary"
	@echo "  build-debug  - Build with debug symbols"
	@echo "  install      - Build and install binary to /usr/local/bin"
	@echo "  clean        - Remove build artifacts"
	@echo "  deps         - Download Go dependencies"
	@echo "  check-deps   - Check system dependencies"
	@echo "  install-deps - Install system dependencies (Ubuntu/Debian)"
	@echo "  test         - Run tests"
	@echo "  run          - Build and run with default settings"
	@echo "  run-dry      - Build and run in dry-run mode"
	@echo "  create-db    - Create example database"
	@echo "  db-info      - Show database information"
	@echo "  help         - Show this help"
	@echo ""
	@echo "Usage examples:"
	@echo "  make build"
	@echo "  make run"
	@echo "  make install"
	@echo "  make run-dry"
	@echo "  make test"