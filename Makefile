PREFIX ?= $(HOME)/.local
VERSION ?= 0.1.0

.PHONY: build test vet install app clean

build:
	go build -o bin/ ./cmd/...

test:
	go test -race ./...

vet:
	go vet ./...

# CLI only (no menu bar app): installs iris + iris-helper side by side.
install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/iris bin/iris-helper $(PREFIX)/bin/
	@echo "now run: $(PREFIX)/bin/iris install"

# macOS: dist/Iris.app with the CLI bundled inside.
app:
	VERSION=$(VERSION) macos/Iris/build-app.sh

clean:
	rm -rf bin dist macos/Iris/.build
