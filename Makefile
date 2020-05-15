# Populate build and version dynamically. These are extracted as scripts to make
# them more easily accessible from Github Actions workflows.
BUILD:=$(shell bin/get-build)
VERSION:=$(shell bin/get-version)

# LDFLAGS is used to inject version information into the binary build. No need
# to keep multiple constants in sync.
LDFLAGS:=-ldflags "-X main.Version=$(VERSION) -X main.Build=$(BUILD)"

.PHONY: all build release test clean
.SILENT: clean
.PRECIOUS: build/%/pcmd build/%/README.md build/%/LICENSE

################################################################################
############################### Main Entrypoints ###############################
################################################################################

all: build test

build: build/pcmd

release: \
	build/pcmd-darwin-amd64.zip \
	build/pcmd-linux-amd64.zip \
	build/pcmd-linux-386.zip \
	build/pcmd-linux-arm64.zip \
	build/pcmd-linux-arm.zip \
	build/pcmd-freebsd-amd64.zip \
	build/pcmd-freebsd-386.zip \
	build/pcmd-freebsd-arm.zip \
	build/pcmd-openbsd-amd64.zip \
	build/pcmd-openbsd-386.zip \
	build/pcmd-openbsd-arm.zip \
	build/pcmd-netbsd-amd64.zip \
	build/pcmd-netbsd-386.zip \
	build/pcmd-netbsd-arm.zip \

test: $(GOFILES)
	go test ${LDFLAGS} ./...

clean:
	rm -r build .pcmd 2> /dev/null || :

################################################################################
################################ Binary Targets ################################
################################################################################

GO_FILES:=$(wildcard *.go)

build/pcmd: $(GO_FILES)
	go build ${LDFLAGS} -o build/pcmd .

# Given the pattern build/pcmd-GOOS-GOARCH, create some helpers to extract
# GOOS and GOARCH
go-os = $(word 2,$(subst -, ,$(word 2,$(subst /, ,$1))))
go-arch = $(word 3,$(subst -, ,$(word 2,$(subst /, ,$1))))

build/%/pcmd: $(GO_FILES)
	GOOS=$(call go-os,$@) GOARCH=$(call go-arch,$@) go build ${LDFLAGS} -o $@ .

################################################################################
############################### Release Bundles ################################
################################################################################

build/%/README.md: README.md
	mkdir -p $(shell dirname $@)
	cp README.md $@

build/%/LICENSE: LICENSE
	mkdir -p $(shell dirname $@)
	cp LICENSE $@

build/%.zip: build/%/pcmd build/%/README.md build/%/LICENSE
	cd build && zip -r $(patsubst build/%.zip,%,$@) $(patsubst build/%.zip,%,$@)
