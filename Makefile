.PHONY: build test ui-test ui-browser-test vet arm64 package clean

GO_BUILD = go build -mod=readonly -trimpath

build:
	mkdir -p build
	$(GO_BUILD) -o build/open25-fne ./cmd/open25-fne

test:
	go test -mod=readonly -race -p 1 ./...
	$(MAKE) ui-test

ui-test:
	node --test internal/frameworkui/*.test.mjs

ui-browser-test:
	OPEN25_ADMIN_BROWSER_TEST=1 go test -mod=readonly ./internal/framework -run '^TestEarlyWebBrowserFixture$$' -count=1
	$(MAKE) ui-test

vet:
	go vet -mod=readonly ./...

arm64:
	mkdir -p build
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO_BUILD) -o build/open25-fne-linux-arm64 ./cmd/open25-fne

package:
	sh tools/framework/package.sh

clean:
	rm -rf build
