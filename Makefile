VERSION ?= $(shell date +%Y.%m.%d)
# Where releases are published, for Whydah to update itself from: the GitHub
# repository of the git remote ("owner/name"). Without one, builds don't update.
REPO ?= $(shell git remote get-url origin 2>/dev/null | sed -nE 's|^.*github\.com[:/]([^/]+/[^/]+)$$|\1|p' | sed 's|\.git$$||')
LDFLAGS := -s -w -X main.version=$(VERSION) -X whydah/internal/selfupdate.repo=$(REPO)
TARGETS := darwin/arm64 darwin/amd64 windows/amd64 windows/arm64 linux/amd64 linux/arm64

.PHONY: build run dist vet e2e release clean app icon winres

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/whydah .

run: build
	./bin/whydah

vet:
	go vet ./...
	GOOS=windows go vet ./...
	GOOS=linux go vet ./...

# End-to-end tests: real pages, browser and helpers (see README).
e2e:
	WHYDAH_E2E=1 go test -count=1 -run TestE2E -timeout 15m ./internal/engine

# One self-contained binary per platform, cross-compiled from any machine.
dist:
	@rm -rf dist && mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		out=dist/whydah-$$os-$$arch$$ext; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out . || exit 1; \
	done

# Whydah.app, a universal (Apple Silicon + Intel) macOS app. Opened from
# Finder it gets its own privacy permissions (e.g. Full Disk Access for browser
# cookies) instead of borrowing the terminal's. Signed with your Apple
# Developer ID if one is in the keychain (what releases use), else with the
# local certificate from scripts/make-signing-cert.sh, so the permissions
# survive rebuilds; with neither, ad-hoc, and they reset each build.
DEVELOPER_ID := $(shell security find-identity -v -p codesigning 2>/dev/null | awk '/"Developer ID Application:/ {print $$2; exit}')
SIGN_ID ?= $(or $(DEVELOPER_ID),Whydah Local Signing)
# The hardened runtime, which notarization requires (release.sh adds a
# secure timestamp).
SIGN_FLAGS ?= --options runtime
APP := bin/Whydah.app

app:
	@rm -rf $(APP) && mkdir -p $(APP)/Contents/MacOS $(APP)/Contents/Resources
	cp packaging/macos/Whydah.icns $(APP)/Contents/Resources/Whydah.icns
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/whydah-arm64 .
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/whydah-amd64 .
	lipo -create -output $(APP)/Contents/MacOS/whydah bin/whydah-arm64 bin/whydah-amd64
	@rm bin/whydah-arm64 bin/whydah-amd64
	sed 's/VERSION/$(VERSION)/g' packaging/macos/Info.plist > $(APP)/Contents/Info.plist
	@if security find-identity -p codesigning | grep -q "$(SIGN_ID)"; then \
		codesign --force $(SIGN_FLAGS) --sign "$(SIGN_ID)" $(APP); \
	else \
		echo "note: no \"$(SIGN_ID)\" certificate; signing ad-hoc (permissions reset on each build)"; \
		codesign --force --sign - $(APP); \
	fi
	@codesign --verify $(APP) && echo "built $(APP)"

# Builds every platform, signs the builds and their checksums, and
# publishes them as a GitHub release (see README, "Releasing").
# DRY=1 stops before publishing.
release:
	./scripts/release.sh $(if $(DRY),--dry-run)

# Redraws the icon in every format (packaging/icon/main.go is the drawing).
# The files it writes are checked in, so builds don't need this.
icon:
	go run ./packaging/icon
	@echo "now run: make winres (to update the Windows .exe icon)"

# Puts the icon into Windows builds: writes rsrc_windows_*.syso, which the Go
# linker picks up for GOOS=windows. Uses github.com/tc-hib/go-winres.
winres:
	go run github.com/tc-hib/go-winres@v0.3.3 simply --icon packaging/windows/whydah.ico --arch amd64,arm64 --out rsrc

clean:
	rm -rf bin dist
