// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The Decred developers
// Heavily inspired by https://github.com/btcsuite/btcd/blob/master/version.go
// Copyright (C) 2015-2022 The Lightning Network Developers

package build

import (
	"fmt"
	"strings"
)

var (
	// Commit stores the current commit of this build, which includes the
	// most recent tag, the number of commits since that tag (if non-zero),
	// the commit hash, and a dirty marker. This should be set using the
	// -ldflags during compilation.
	Commit string

	// CommitHash stores the current commit hash of this build, this should
	// be set using the -ldflags during compilation.
	CommitHash string

	// RawTags contains the raw set of build tags, separated by commas. This
	// should be set using -ldflags during compilation.
	RawTags string

	// GoVersion stores the go version that the executable was compiled
	// with. This should be set using -ldflags during compilation.
	GoVersion string
)

// semanticAlphabet is the set of characters that are permitted for use in an
// AppPreRelease.
const semanticAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-."

// These constants define the application version and follow the semantic
// versioning 2.0.0 spec (http://semver.org/).
const (
	// AppMajor defines the major version of this binary.
	AppMajor uint = 0

	// AppMinor defines the minor version of this binary.
	AppMinor uint = 14

	// AppPatch defines the application patch for this binary.
	AppPatch uint = 99

	// AppPreRelease MUST only contain characters from semanticAlphabet
	// per the semantic versioning spec.
	AppPreRelease = "beta"
)

func init() {
	// Assert that AppPreRelease is valid according to the semantic
	// versioning guidelines for pre-release version and build metadata
	// strings. In particular it MUST only contain characters in
	// semanticAlphabet.
	for _, r := range AppPreRelease {
		if !strings.ContainsRune(semanticAlphabet, r) {
			panic(fmt.Errorf("rune: %v is not in the semantic "+
				"alphabet", r))
		}
	}
}

// Version returns the application version as a properly formed string per the
// semantic versioning 2.0.0 spec (http://semver.org/).
func Version() string {
	// Start with the major, minor, and patch versions.
	version := fmt.Sprintf("%d.%d.%d", AppMajor, AppMinor, AppPatch)

	// Append pre-release version if there is one. The hyphen called for by
	// the semantic versioning spec is automatically appended and should not
	// be contained in the pre-release string.
	if AppPreRelease != "" {
		version = fmt.Sprintf("%s-%s", version, AppPreRelease)
	}

	return version
}

// Tags returns the list of build tags that were compiled into the executable.
func Tags() []string {
	if len(RawTags) == 0 {
		return nil
	}

	return strings.Split(RawTags, ",")
}
