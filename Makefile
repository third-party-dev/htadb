# htadb build. Everything builds with Go alone; Node is needed only for
# `vsce` when packaging the VS Code extension (and for the JS unit tests).
#
#   make fetchdeps                 pinned xterm.js + newest 4.x/5.0 server jars
#   make htadb [WEB=dev|min]       host build (min by default) → bin/htadb
#   make htadb-all [TARGETS=...]   CGO_ENABLED=0 cross builds → bin/<os>-<arch>/
#   make vsix [TARGETS=...]        one vsix carrying every target's binary
#   make vsix-per-platform         one small vsix per platform (vsce --target)
#   make test                      Go tests + JS unit tests + browser smoke test
#   make dist                      source archive, binaries, vsix(s), SHA256SUMS
#
# Without a local Go toolchain, run any target in the dev container:
#   docker build -t htadb-dev .devcontainer && .devcontainer/run make test

GO       ?= go
WEB      ?= min
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TARGETS  ?= linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
GOTAGS   ?=
VSCE     ?= npx --yes @vscode/vsce@3.9.2
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOFLAGS_BUILD := -trimpath -ldflags "$(LDFLAGS)" $(if $(GOTAGS),-tags $(GOTAGS))

JARS     := internal/mirror/servers/embedded/scrcpy-server-v5.0.1.jar internal/mirror/servers/embedded/scrcpy-server-v4.1.jar
XTERM    := web/vendor/xterm/xterm.js

.PHONY: all fetchdeps web htadb htadb-all vsix vsix-per-platform test test-go test-js test-e2e dist clean vet

all: htadb

fetchdeps: $(XTERM) $(JARS)

$(XTERM) $(JARS):
	cd tools && $(GO) run ./fetchdeps -root ..

# Web assets for embedding: dev copies sources through, min bundles+minifies.
web: $(XTERM)
	cd tools && $(GO) run ./webbuild -src ../web -out ../internal/ui/dist -mode $(WEB)

htadb: web
	CGO_ENABLED=0 $(GO) build $(GOFLAGS_BUILD) -o bin/htadb$(shell $(GO) env GOEXE) ./cmd/htadb

htadb-all: web
	@set -e; for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "build $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build $(GOFLAGS_BUILD) -o bin/$$os-$$arch/htadb$$ext ./cmd/htadb; \
	done

# VS Code target names for each GOOS/GOARCH.
vscode_target = $(subst linux/amd64,linux-x64,$(subst linux/arm64,linux-arm64,$(subst linux/arm,linux-armhf,$(subst darwin/amd64,darwin-x64,$(subst darwin/arm64,darwin-arm64,$(subst windows/amd64,win32-x64,$(subst windows/arm64,win32-arm64,$(1))))))))

vsix: htadb-all
	rm -rf vscode-extension/bin && mkdir -p vscode-extension/bin dist
	@set -e; for t in $(TARGETS); do os=$${t%/*}; arch=$${t#*/}; cp -r bin/$$os-$$arch vscode-extension/bin/; done
	cd vscode-extension && $(VSCE) package --no-dependencies --skip-license --allow-missing-repository -o ../dist/htadb-$(VERSION).vsix

vsix-per-platform: htadb-all
	@set -e; mkdir -p dist; for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		vt=$$(echo $$t | sed -e 's#linux/amd64#linux-x64#;s#linux/arm64#linux-arm64#;s#linux/arm#linux-armhf#;s#darwin/amd64#darwin-x64#;s#darwin/arm64#darwin-arm64#;s#windows/amd64#win32-x64#;s#windows/arm64#win32-arm64#'); \
		rm -rf vscode-extension/bin && mkdir -p vscode-extension/bin && cp -r bin/$$os-$$arch vscode-extension/bin/; \
		(cd vscode-extension && $(VSCE) package --no-dependencies --skip-license --allow-missing-repository --target $$vt -o ../dist/htadb-$$vt-$(VERSION).vsix); \
	done

vet:
	$(GO) vet ./...
	cd tools && $(GO) vet ./...

test: test-go htadb test-js test-e2e

test-go: vet
	$(GO) test ./...
	$(GO) test -tags nomirrorjar ./internal/mirror/...

test-js:
	node --test web/test/
	$(GO) build -o bin/fakeadb ./internal/adb/adbtest/fakeadb
	node --test vscode-extension/test/

# Browser smoke test: fake adb + fake device streaming an ffmpeg-generated
# H.264 clip, headless Chromium loading the embedded pages.
test-e2e: htadb
	./e2e/run.sh

dist:
	rm -rf dist
	$(MAKE) vsix
	@set -e; mkdir -p dist; \
	git archive --format=tar.gz --prefix=htadb-$(VERSION)/ -o dist/htadb-$(VERSION)-src.tar.gz HEAD; \
	for t in $(TARGETS); do os=$${t%/*}; arch=$${t#*/}; \
		(cd bin/$$os-$$arch && cp ../../internal/mirror/servers/embedded/LICENSE-scrcpy ../../internal/mirror/servers/embedded/NOTICE-scrcpy . && \
		 if [ $$os = windows ]; then zip -q ../../dist/htadb-$(VERSION)-$$os-$$arch.zip *; else tar czf ../../dist/htadb-$(VERSION)-$$os-$$arch.tar.gz *; fi); \
	done; \
	cd dist && sha256sum $$(ls | grep -v SHA256SUMS) > SHA256SUMS && cat SHA256SUMS

clean:
	rm -rf bin dist vscode-extension/bin testdata-gen
	find internal/ui/dist -mindepth 1 ! -name .keep -exec rm -rf {} +
