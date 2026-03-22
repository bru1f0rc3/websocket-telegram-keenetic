APP_NAME = tg-ws-proxy
VERSION = 2.0.0
CMD = ./cmd/tg-ws-proxy

# Go build flags for small static binaries
LDFLAGS = -s -w -X main.version=$(VERSION)
GCFLAGS =
BUILD_FLAGS = -trimpath -ldflags "$(LDFLAGS)"

# Output directory
DIST = dist

.PHONY: all clean build linux windows darwin \
        keenetic-mips keenetic-arm entware \
        install-service help

all: build

build:
	go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)$(shell go env GOEXE) $(CMD)

# === Native platforms ===

windows-amd64:
	GOOS=windows GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_windows_amd64.exe $(CMD)

windows-arm64:
	GOOS=windows GOARCH=arm64 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_windows_arm64.exe $(CMD)

linux-amd64:
	GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_linux_amd64 $(CMD)

linux-arm64:
	GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_linux_arm64 $(CMD)

linux-arm:
	GOOS=linux GOARCH=arm GOARM=7 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_linux_armv7 $(CMD)

darwin-amd64:
	GOOS=darwin GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_darwin_amd64 $(CMD)

darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_darwin_arm64 $(CMD)

freebsd-amd64:
	GOOS=freebsd GOARCH=amd64 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_freebsd_amd64 $(CMD)

# === Keenetic / Entware targets ===

# Keenetic mipsel: Viva, Omni, Extra, Giga, Ultra, Giant, Hero 4G, Hopper (KN-3810) etc.
keenetic-mipsel:
	GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_keenetic_mipsel $(CMD)

# Keenetic aarch64: Peak, Ultra (KN-1811), Giga (KN-1012), Hopper (KN-3811/3812)
keenetic-aarch64:
	GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_keenetic_aarch64 $(CMD)

# Keenetic mips: Ultra SE, Giga SE, DSL, Skipper DSL, Duo, Hopper DSL etc.
keenetic-mips:
	GOOS=linux GOARCH=mips GOMIPS=softfloat go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_keenetic_mips $(CMD)

# Generic Entware ARM (armv7 softfloat) 
entware-arm:
	GOOS=linux GOARCH=arm GOARM=7 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_entware_armv7 $(CMD)

# Entware MIPS (for routers like Asus RT-AC series)
entware-mipsel:
	GOOS=linux GOARCH=mipsle GOMIPS=softfloat go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_entware_mipsel $(CMD)

# Entware aarch64
entware-aarch64:
	GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -o $(DIST)/$(APP_NAME)_entware_aarch64 $(CMD)

# === Build all ===

all-platforms: windows-amd64 windows-arm64 \
               linux-amd64 linux-arm64 linux-arm \
               darwin-amd64 darwin-arm64 \
               freebsd-amd64 \
               keenetic-mipsel keenetic-aarch64 keenetic-mips \
               entware-arm entware-mipsel entware-aarch64

# === Entware package helpers ===

entware: keenetic-mipsel keenetic-aarch64 entware-arm entware-mipsel entware-aarch64 keenetic-mips

clean:
	rm -rf $(DIST)

# === Testing ===

test:
	go test ./...

vet:
	go vet ./...

help:
	@echo ""
	@echo "TG WS Proxy — Makefile targets"
	@echo ""
	@echo "  build              Build for current OS/arch"
	@echo "  all-platforms      Build for all supported platforms"
	@echo ""
	@echo "  windows-amd64      Windows x86_64"
	@echo "  windows-arm64      Windows ARM64"
	@echo "  linux-amd64        Linux x86_64"
	@echo "  linux-arm64        Linux ARM64"
	@echo "  linux-arm          Linux ARMv7"
	@echo "  darwin-amd64       macOS Intel"
	@echo "  darwin-arm64       macOS Apple Silicon"
	@echo "  freebsd-amd64      FreeBSD x86_64"
	@echo ""
	@echo "  Keenetic / Entware:"
	@echo "  keenetic-mipsel    Keenetic Viva (MT7621, MIPSel)"
	@echo "  keenetic-aarch64   Keenetic Hopper (ARM64)"
	@echo "  keenetic-mips      Keenetic older (MIPS BE)"
	@echo "  entware-arm        Entware ARMv7"
	@echo "  entware-mipsel     Entware MIPSel"
	@echo "  entware-aarch64    Entware ARM64"
	@echo "  entware            All Entware/Keenetic targets"
	@echo ""
	@echo "  Service management (after installing binary):"
	@echo "  ./tg-ws-proxy --service install"
	@echo "  ./tg-ws-proxy --service start"
	@echo "  ./tg-ws-proxy --service stop"
	@echo "  ./tg-ws-proxy --service uninstall"
	@echo ""
