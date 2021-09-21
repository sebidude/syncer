.PHONY: all test clean
.ONESHELL:

APPNAME := syncer
APPSRC := ./cmd/$(APPNAME)

GITCOMMITHASH := $(shell git log --max-count=1 --pretty="format:%h" HEAD)
GITCOMMIT := -X main.gitcommit=$(GITCOMMITHASH)

VERSIONTAG := $(shell git describe --tags --abbrev=0)
VERSION := -X main.appversion=$(VERSIONTAG)

BUILDTIMEVALUE := $(shell date +%Y-%m-%dT%H:%M:%S%z)
BUILDTIME := -X main.buildtime=$(BUILDTIMEVALUE)

LDFLAGS := '-extldflags "-static" -d -s -w $(GITCOMMIT) $(VERSION) $(BUILDTIME)'

all:info clean build-linux image

clean:
	rm -rf build

info: 
	@echo - appname:   $(APPNAME)
	@echo - version:   $(VERSIONTAG)
	@echo - commit:    $(GITCOMMITHASH)
	@echo - buildtime: $(BUILDTIMEVALUE) 

dep:
	@go get -v -d ./...

build-linux: info dep
	@echo Building for linux
	@mkdir -p build/linux
	@CGO_ENABLED=0 \
	GOOS=linux \
	go build -o build/linux/$(APPNAME)-$(VERSIONTAG)-$(GITCOMMITHASH) -a -ldflags $(LDFLAGS) $(APPSRC)
	@cp build/linux/$(APPNAME)-$(VERSIONTAG)-$(GITCOMMITHASH) build/linux/$(APPNAME)

install:
	@cp build/linux/$(APPNAME) ${GOPATH}/bin/$(APPNAME)

image: 
	@echo Creating docker image
	@docker build -t $(APPNAME):latest -t $(APPNAME):$(VERSIONTAG)-$(GITCOMMITHASH) .

tarball:
	@echo Creating tarball
	@cd build/linux
	@tar cvfz syncer-$(VERSIONTAG).linux-amd64.tar.gz syncer
	@sha256sum syncer-$(VERSIONTAG).linux-amd64.tar.gz > syncer-$(VERSIONTAG).linux-amd64.tar.gz.sha256

