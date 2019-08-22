PKG := github.com/lightningnetwork/lnd

GOINSTALL := GO111MODULE=on go install -v

COMMIT := $(shell git describe --abbrev=40 --dirty)
LDFLAGS := -ldflags "-X $(PKG)/build.Commit=$(COMMIT)"

install:
	@$(call print, "Installing towerd.")
	$(GOINSTALL) -tags="${tags}" $(LDFLAGS) $(PKG)/cmd/towerd
