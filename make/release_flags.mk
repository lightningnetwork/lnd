VERSION_TAG = $(shell date +%Y%m%d)-01
VERSION_CHECK = @$(call print, "Building master with date version tag")

BUILD_SYSTEM = darwin-386 \
darwin-amd64 \
dragonfly-amd64 \
freebsd-386 \
freebsd-amd64 \
freebsd-arm \
illumos-amd64 \
linux-386 \
linux-amd64 \
linux-armv6 \
linux-armv7 \
linux-arm64 \
linux-ppc64 \
linux-ppc64le \
linux-mips \
linux-mipsle \
linux-mips64 \
linux-mips64le \
linux-s390x \
netbsd-386 \
netbsd-amd64 \
netbsd-arm \
netbsd-arm64 \
openbsd-386 \
openbsd-amd64 \
openbsd-arm \
openbsd-arm64 \
solaris-amd64 \
windows-386 \
windows-amd64 \
windows-arm

RELEASE_TAGS = autopilotrpc signrpc walletrpc chainrpc invoicesrpc watchtowerrpc

# One can either specify a git tag as the version suffix or one is generated
# from the current date.
ifneq ($(tag),)
VERSION_TAG = $(tag)
VERSION_CHECK = ./scripts/release.sh check-tag "$(VERSION_TAG)"
endif

# By default we will build all systems. But with the 'sys' tag, a specific
# system can be specified. This is useful to release for a subset of
# systems/architectures.
ifneq ($(sys),)
BUILD_SYSTEM = $(sys)
endif

# Use all build tags by default but allow them to be overwritten.
ifneq ($(tags),)
RELEASE_TAGS = $(tags)
endif
