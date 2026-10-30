PLUGIN_ID ?= prompt-filter
BUILD_DIR ?= bin
EXT := $(shell go env GOOS | sed -e 's/linux/so/' -e 's/darwin/dylib/' -e 's/windows/dll/')
TARGET := $(BUILD_DIR)/$(PLUGIN_ID).$(EXT)

# Directory the gateway scans for native plugins.
PLUGIN_DIR ?= /opt/cliproxyapi/plugins

.PHONY: all build test vet install clean

all: test build

build:
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o $(TARGET) .
	@rm -f $(BUILD_DIR)/$(PLUGIN_ID).h

test:
	go test ./...

vet:
	go vet ./...

# Copy the artifact next to other plugins and remove the generated header.
install: build
	@test -d $(PLUGIN_DIR) || (echo "plugin dir not found: $(PLUGIN_DIR)" && exit 1)
	install -m 0644 $(TARGET) $(PLUGIN_DIR)/$(PLUGIN_ID).$(EXT)
	@echo "installed $(PLUGIN_DIR)/$(PLUGIN_ID).$(EXT)"

clean:
	rm -rf $(BUILD_DIR)
