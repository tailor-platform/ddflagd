PKG = github.com/k1LoW/ddflagd
COMMIT = $$(git describe --tags --always)
OSNAME=${shell uname -s}
ifeq ($(OSNAME),Darwin)
	DATE = $$(gdate --utc '+%Y-%m-%d_%H:%M:%S')
else
	DATE = $$(date --utc '+%Y-%m-%d_%H:%M:%S')
endif

export GO111MODULE=on

# commit and date live in package main of cmd/ddflagd, which the linker
# addresses as "main".
BUILD_LDFLAGS = -s -w -X main.commit=$(COMMIT) -X main.date=$(DATE)

default: test

ci: depsdev test

test:
	go test ./... -coverprofile=coverage.out -covermode=count

# e2e needs the fake Agent, so it is kept out of the default test run.
e2e:
	docker compose up -d --wait test-agent
	go test -tags e2e ./e2e/... -timeout 900s

# The Rust tests point the official crates at ddflagd, so they need the stub
# server built first.
rust-test:
	go build -o rust-integration/target/ofrepstub ./rust-integration/ofrepstub
	cd rust-integration && DDFLAGD_OFREP_STUB=$(CURDIR)/rust-integration/target/ofrepstub cargo test

lint:
	golangci-lint run ./...
	govulncheck ./...
	go vet -vettool=`which gostyle` -gostyle.config=$(PWD)/.gostyle.yml ./...

build:
	CGO_ENABLED=0 go build -ldflags="$(BUILD_LDFLAGS)" -o ddflagd ./cmd/ddflagd

depsdev:
	go install github.com/Songmu/gocredits/cmd/gocredits@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/k1LoW/gostyle@latest

prerelease_for_tagpr: depsdev
	go mod tidy
	gocredits -skip-missing -w .
	git add CHANGELOG.md CREDITS go.mod go.sum

.PHONY: default ci test e2e rust-test lint build depsdev prerelease_for_tagpr
