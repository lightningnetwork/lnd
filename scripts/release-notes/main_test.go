package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFoldFragment checks that fragment entries are appended to the section
// with the same heading path and that the rest of the document is unchanged.
func TestFoldFragment(t *testing.T) {
	t.Parallel()

	doc := []string{
		"# Release Notes",
		"",
		"# Bug Fixes",
		"",
		"* existing fix",
		"",
		"",
		"# Improvements",
		"",
		"## Code Health",
		"",
		"## Deprecations",
		"",
		"### Warning",
		"",
		"# Technical and Architectural Updates",
		"",
		"## Code Health",
		"",
		"```text",
		"# not a heading",
		"```",
		"",
		"# Contributors (Alphabetical Order)",
	}

	tests := []struct {
		name     string
		fragment []string
		want     []string
		err      error
	}{{
		name: "append to non-empty section",
		fragment: []string{
			"# Bug Fixes", "", "* new fix", "  continued", "",
		},
		want: []string{
			"# Release Notes", "",
			"# Bug Fixes", "", "* existing fix", "",
			"* new fix", "  continued", "",
			"# Improvements", "", "## Code Health", "",
			"## Deprecations", "", "### Warning", "",
			"# Technical and Architectural Updates", "",
			"## Code Health", "",
			"```text", "# not a heading", "```", "",
			"# Contributors (Alphabetical Order)",
		},
	}, {
		name: "same heading under different parents",
		fragment: []string{
			"# Technical and Architectural Updates", "",
			"## Code Health", "", "* technical",
			"# Improvements", "", "## Code Health", "",
			"* improvement",
		},
		want: []string{
			"# Release Notes", "",
			"# Bug Fixes", "", "* existing fix", "", "",
			"# Improvements", "", "## Code Health", "",
			"* improvement", "",
			"## Deprecations", "", "### Warning", "",
			"# Technical and Architectural Updates", "",
			"## Code Health", "",
			"```text", "# not a heading", "```", "",
			"* technical", "",
			"# Contributors (Alphabetical Order)",
		},
	}, {
		name: "entries go above nested headings",
		fragment: []string{
			"# Improvements", "", "## Deprecations", "",
			"* deprecated",
		},
		want: []string{
			"# Release Notes", "",
			"# Bug Fixes", "", "* existing fix", "", "",
			"# Improvements", "", "## Code Health", "",
			"## Deprecations", "", "* deprecated", "",
			"### Warning", "",
			"# Technical and Architectural Updates", "",
			"## Code Health", "",
			"```text", "# not a heading", "```", "",
			"# Contributors (Alphabetical Order)",
		},
	}, {
		name: "headings in fragment code blocks are entries",
		fragment: []string{
			"# Bug Fixes", "", "```text", "# Missing", "```",
		},
		want: []string{
			"# Release Notes", "",
			"# Bug Fixes", "", "* existing fix", "",
			"```text", "# Missing", "```", "",
			"# Improvements", "", "## Code Health", "",
			"## Deprecations", "", "### Warning", "",
			"# Technical and Architectural Updates", "",
			"## Code Health", "",
			"```text", "# not a heading", "```", "",
			"# Contributors (Alphabetical Order)",
		},
	}, {
		name: "missing section",
		fragment: []string{
			"# Bug Fixes", "", "## Unknown", "", "* entry",
		},
		err: errSectionNotFound,
	}, {
		name:     "text before the first heading",
		fragment: []string{"* entry", "", "# Bug Fixes"},
		err:      errTextBeforeHeading,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := foldFragment(doc, test.fragment)
			if test.err != nil {
				require.ErrorIs(t, err, test.err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

// TestFoldFragmentsInOrder checks that entries of several fragments in the
// same section keep the order in which the fragments are folded.
func TestFoldFragmentsInOrder(t *testing.T) {
	t.Parallel()

	doc := []string{"# Bug Fixes", "", "# New Features"}
	for _, entry := range []string{"* first", "* second"} {
		var err error
		doc, err = foldFragment(doc, []string{"# Bug Fixes", "", entry})
		require.NoError(t, err)
	}

	require.Equal(t, []string{
		"# Bug Fixes", "", "* first", "", "* second", "",
		"# New Features",
	}, doc)
}

// TestContributors checks that the contributor list merges the listed names
// with the Git authors.
func TestContributors(t *testing.T) {
	t.Parallel()

	doc := []string{
		"# Bug Fixes", "",
		"# Contributors (Alphabetical Order)", "",
		"* Ziggie", "* alice",
	}
	authors := []string{
		"ziggie", "Bob", "dependabot[bot]", "Alice", "", "Carol",
	}

	names := mergeNames(contributors(doc), authors)
	require.Equal(t, []string{"alice", "Bob", "Carol", "Ziggie"}, names)

	require.Equal(t, []string{
		"# Bug Fixes", "",
		"# Contributors (Alphabetical Order)", "",
		"* alice", "* Bob", "* Carol", "* Ziggie",
	}, setContributors(doc, names))
}

// TestSetContributors checks where the contributor list is written.
func TestSetContributors(t *testing.T) {
	t.Parallel()

	// A section after the contributors stays in place.
	doc := []string{
		"# Contributors (Alphabetical Order)", "", "* Old", "",
		"# Appendix",
	}
	require.Equal(t, []string{
		"# Contributors (Alphabetical Order)", "", "* New", "",
		"# Appendix",
	}, setContributors(doc, []string{"New"}))

	// A document without contributors section gets one appended.
	doc = []string{"# Bug Fixes", "", "* fix", ""}
	require.Equal(t, []string{
		"# Bug Fixes", "", "* fix", "",
		"# Contributors (Alphabetical Order)", "", "* New",
	}, setContributors(doc, []string{"New"}))
}

// TestLinkedPRs checks that PR links are matched by their exact number.
func TestLinkedPRs(t *testing.T) {
	t.Parallel()

	content := strings.Join([]string{
		"[a](https://github.com/lightningnetwork/lnd/pull/11327)",
		"[b](https://github.com/lightningnetwork/lnd/pull/113270)",
		"[c](https://github.com/lightning/bolts/pull/1059)",
	}, "\n")

	require.Equal(t, map[int]bool{11327: true, 113270: true},
		linkedPRs(content))
}

// TestPreviousRelease checks that the default start of the contributor range
// is the newest final release below the version, regardless of ancestry.
func TestPreviousRelease(t *testing.T) {
	t.Parallel()

	tags := []string{
		"v0.20.0-beta", "v0.20.1-beta", "v0.21.0-beta",
		"v0.21.4-beta", "v0.21.4-beta-zeus", "v0.21.5-beta.rc1",
		"v0.21.10-beta", "v0.22.0-beta",
	}

	tests := []struct {
		version string
		want    string
	}{
		{version: "0.22.0", want: "v0.21.10-beta"},
		{version: "0.21.5", want: "v0.21.4-beta"},
		{version: "0.21.0", want: "v0.20.1-beta"},
		{version: "0.23.1", want: "v0.22.0-beta"},
	}
	for _, test := range tests {
		got, err := previousRelease(test.version, tags)
		require.NoError(t, err)
		require.Equal(t, test.want, got, test.version)
	}

	_, err := previousRelease("0.20.0", tags)
	require.Error(t, err)
}

// TestCanonicalNames checks that listed names are mapped to their .mailmap
// name if they are a Git author alias.
func TestCanonicalNames(t *testing.T) {
	t.Parallel()

	authors, aliases := parseAuthors([]string{
		"Oli\x00Oliver Gugger",
		"Bob\x00Bob",
		"Bobby\x00Bob",
		"Alice\x00Alice Doe",
		"Alice\x00Alice",
		"malformed",
	})
	require.Equal(t, []string{
		"Oliver Gugger", "Bob", "Bob", "Alice Doe", "Alice",
	}, authors)
	require.Equal(t, map[string]string{
		"oli": "Oliver Gugger", "bobby": "Bob",
	}, aliases)

	// The alias listed by an earlier fold is replaced, while a co-author
	// added by hand and an ambiguous name are kept.
	listed := canonicalNames([]string{"oli", "Carol", "Alice"}, aliases)
	require.Equal(t, []string{"Oliver Gugger", "Carol", "Alice"}, listed)

	require.Equal(t, []string{
		"Alice", "Alice Doe", "Bob", "Carol", "Oliver Gugger",
	}, mergeNames(listed, authors))
}
