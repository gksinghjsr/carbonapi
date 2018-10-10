UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
	PKGCONF = PKG_CONFIG_PATH="/opt/X11/lib/pkgconfig"
else
	PKGCONF =
endif

GO ?= go
VERSION ?= $(shell git rev-parse --short HEAD)

# List packages and source files

PKG_CARBONAPI=gitlab.booking.com/graphite/carbonapi/cmd/carbonapi
PKG_CARBONZIPPER=gitlab.booking.com/graphite/carbonapi/cmd/carbonzipper
SOURCES = $(shell find . -name '*.go')

# Set compile flags

GCFLAGS :=
debug: GCFLAGS += -gcflags=all='-l -N'

LDFLAGS = -ldflags '-X main.BuildVersion=$(VERSION)'

TAGS := -tags cairo
nocairo: TAGS =

# Define targets

all: $(SOURCES) build

.PHONY: debug
debug: build

nocairo: $(SOURCES) build

build:
	$(PKGCONF) $(GO) build $(TAGS) $(LDFLAGS) $(GCFLAGS) $(PKG_CARBONAPI)
	$(PKGCONF) $(GO) build $(TAGS) $(LDFLAGS) $(GCFLAGS) $(PKG_CARBONZIPPER)

vet:
	go vet -composites=false ./...

test:
	$(PKGCONF) $(GO) test $(TAGS) ./... -race -coverprofile cover.out

clean:
	rm -f carbonapi carbonzipper
