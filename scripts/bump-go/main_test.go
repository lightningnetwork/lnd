package main

import (
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// memTree is an in-memory tree for tests.
type memTree map[string]string

func (m memTree) files() []string {
	var out []string
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)

	return out
}

func (m memTree) read(p string) (string, error) {
	return m[p], nil
}

func (m memTree) write(p, content string) error {
	m[p] = content
	return nil
}

// sum returns a fake but well-formed SHA256 for an archive name.
func sum(name string) string {
	h := strings.Repeat("0", 64)
	for i, c := range name {
		h = h[:i%64] + string("0123456789abcdef"[int(c)%16]) +
			h[i%64+1:]
	}

	return h
}

// testReleases returns a go.dev feed with the given stable versions, plus
// 1.27.5 listed as unstable.
func testReleases(versions ...string) *releases {
	rel := &releases{
		stable:   map[string]bool{"1.27.5": false},
		sums:     map[string]string{},
		archives: map[string]archive{},
	}
	for _, v := range versions {
		rel.stable[v] = true
		for _, plat := range []string{
			"linux-amd64.tar.gz", "linux-armv6l.tar.gz",
		} {
			name := "go" + v + "." + plat
			h := sum(name)
			rel.sums[name] = h
			rel.archives[h] = archive{v, plat}
		}
	}

	return rel
}

func installDocFor(v string) string {
	amd := "go" + v + ".linux-amd64.tar.gz"
	arm := "go" + v + ".linux-armv6l.tar.gz"

	return strings.Join([]string{
		"`lnd` is written in Go, with a minimum version of `" + v +
			"`.",
		"  wget https://dl.google.com/go/" + amd,
		`  echo "` + sum(amd) + "  " + amd + `" | sha256sum --check`,
		"  wget https://dl.google.com/go/" + arm,
		`  echo "` + sum(arm) + "  " + arm + `" | sha256sum --check`,
		"",
	}, "\n")
}

// testTree returns a consistent tree with one pin for every rule.
func testTree(tc, minV string) memTree {
	return memTree{
		"Makefile": "GO_VERSION = " + tc + "\n",
		".github/workflows/main.yml": "env:\n  GO_VERSION: " + tc +
			"\n",
		".golangci.yml": "run:\n  go: \"" + tc + "\"\n",
		"Dockerfile": "FROM golang:" + tc +
			"-alpine as builder\n",
		"tools/Dockerfile": "FROM golang:" + tc + "\nRUN true\n",
		"lnrpc/gen_protos_docker.sh": "GO_IMAGE=docker.io/library/" +
			"golang:" + tc + "-alpine\n",
		".github/actions/setup-go/action.yml": "# Example: " +
			"`linux-go-" + tc + "-unit-test-`.\n# with Go " +
			majorOf(tc) + " only looks for\n",
		"mobile/README.md": "wget -c https://dl.google.com/go/go" + tc +
			".linux-amd64.tar.gz\n",
		"docs/ci-runs.md": "jobs install Go " + tc + " through\n",
		"go.mod":          "module x\n\ngo " + minV + "\n",
		"tlv/go.mod":      "module y\n\ngo " + minV + "\n",
		"sqldb/v2/go.mod": "module z\n\ngo " + minV + "\n",
		installDoc:        installDocFor(minV),
		"main.go":         "package main\n",
	}
}

func noLeftovers(string) ([]string, error) {
	return nil, nil
}

// TestStatusConsistent checks that a consistent tree passes and that every
// rule finds its pin.
func TestStatusConsistent(t *testing.T) {
	t.Parallel()

	rep, err := check(testTree("1.27.3", "1.26.10"),
		testReleases("1.27.3", "1.26.10"))
	require.NoError(t, err)
	require.Empty(t, rep.failures)
	require.Equal(t, "1.27.3", rep.current[toolchain])
	require.Equal(t, "1.26.10", rep.current[minimum])
}

// TestStatusFailures checks the ways a tree can be inconsistent.
func TestStatusFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(memTree)
		want   string
	}{{
		name: "drifted go.mod",
		mutate: func(m memTree) {
			m["tlv/go.mod"] = "module y\n\ngo 1.19\n"
		},
		want: "tlv/go.mod:3 has 1.19, min is 1.26.10",
	}, {
		name: "stale major-only pin",
		mutate: func(m memTree) {
			p := ".github/actions/setup-go/action.yml"
			m[p] = "# Example: `linux-go-1.27.3-unit-test-`.\n" +
				"# with Go 1.26 only looks for\n"
		},
		want: "has 1.26, toolchain is 1.27",
	}, {
		name: "checksum of the wrong version",
		mutate: func(m memTree) {
			m[installDoc] = strings.Replace(m[installDoc],
				sum("go1.26.10.linux-amd64.tar.gz"),
				sum("go1.27.3.linux-amd64.tar.gz"), 1)
		},
		want: "checksum is for go1.27.3.linux-amd64.tar.gz",
	}, {
		name: "unknown checksum",
		mutate: func(m memTree) {
			m[installDoc] = strings.Replace(m[installDoc],
				sum("go1.26.10.linux-amd64.tar.gz"),
				strings.Repeat("f", 64), 1)
		},
		want: "is not a go.dev archive checksum",
	}, {
		name: "pinned file changed format",
		mutate: func(m memTree) {
			m[".golangci.yml"] = "run:\n  go: '1.27.3'\n"
		},
		want: "matched nothing",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tree := testTree("1.27.3", "1.26.10")
			tc.mutate(tree)
			rep, err := check(tree,
				testReleases("1.27.3", "1.26.10"))
			require.NoError(t, err)
			require.NotEmpty(t, rep.failures)
			require.Contains(t, strings.Join(rep.failures, "\n"),
				tc.want)
		})
	}
}

// TestStatusMissingFile checks that a rule whose files don't exist is
// skipped, as on trees that predate docs/ci-runs.md.
func TestStatusMissingFile(t *testing.T) {
	t.Parallel()

	tree := testTree("1.27.3", "1.26.10")
	delete(tree, "docs/ci-runs.md")
	rep, err := check(tree, testReleases("1.27.3", "1.26.10"))
	require.NoError(t, err)
	require.Empty(t, rep.failures)
}

// TestSetMajorBump checks that a major bump of both versions rewrites every
// pin, including the major-only one, and the checksums.
func TestSetMajorBump(t *testing.T) {
	t.Parallel()

	tree := testTree("1.26.6", "1.25.13")
	rel := testReleases("1.26.6", "1.25.13", "1.27.1", "1.26.8")
	changed, err := set(io.Discard, tree, rel, map[kind]string{
		toolchain: "1.27.1", minimum: "1.26.8",
	}, false, noLeftovers)
	require.NoError(t, err)

	want := testTree("1.27.1", "1.26.8")
	require.Equal(t, want, tree)
	require.Len(t, changed, len(want)-1, "every file but main.go")
}

// TestSetToolchainOnly checks that bumping the toolchain leaves the minimum
// alone, even when both hold the same version.
func TestSetToolchainOnly(t *testing.T) {
	t.Parallel()

	tree := testTree("1.27.3", "1.27.3")
	_, err := set(io.Discard, tree, testReleases("1.27.3", "1.27.4"),
		map[kind]string{toolchain: "1.27.4"}, false, noLeftovers)
	require.NoError(t, err)

	want := testTree("1.27.4", "1.27.3")
	require.Equal(t, want, tree)
}

// TestSetOldChecksumFormat checks that checksums are rewritten by value, in
// the INSTALL.md format used before 7fcbbd44ca.
func TestSetOldChecksumFormat(t *testing.T) {
	t.Parallel()

	oldDoc := func(v string) string {
		amd := "go" + v + ".linux-amd64.tar.gz"

		return "minimum version of `" + v + "`\n" +
			"  sha256sum " + amd + " | awk '{ print $1 }'\n" +
			"  The final output of the command above should be\n" +
			"  `" + sum(amd) + "`. If it\n"
	}
	tree := testTree("1.27.3", "1.26.10")
	tree[installDoc] = oldDoc("1.26.10")
	rel := testReleases("1.27.3", "1.26.10")
	_, err := set(io.Discard, tree, rel,
		map[kind]string{minimum: "1.27.3"}, false, noLeftovers)
	require.NoError(t, err)
	require.Equal(t, oldDoc("1.27.3"), tree[installDoc])
}

// TestSetInconsistent checks that a drifted tree is refused, and that
// -allow-inconsistent only rewrites pins on the anchor version.
func TestSetInconsistent(t *testing.T) {
	t.Parallel()

	drifted := func() memTree {
		tree := testTree("1.27.3", "1.26.10")
		tree["tlv/go.mod"] = "module y\n\ngo 1.19\n"

		return tree
	}
	rel := testReleases("1.27.3", "1.26.10", "1.26.11")
	want := map[kind]string{minimum: "1.26.11"}

	tree := drifted()
	_, err := set(io.Discard, tree, rel, want, false, noLeftovers)
	require.ErrorContains(t, err, "refusing to bump")
	require.Equal(t, drifted(), tree, "nothing written")

	tree = drifted()
	_, err = set(io.Discard, tree, rel, want, true, noLeftovers)
	require.ErrorContains(t, err, "check(s) failed",
		"the drifted pin still fails the final check")
	require.Equal(t, "module x\n\ngo 1.26.11\n", tree["go.mod"])
	require.Equal(t, "module y\n\ngo 1.19\n", tree["tlv/go.mod"])
}

// TestSetRejectsVersions checks the version validation.
func TestSetRejectsVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want map[kind]string
		err  string
	}{{
		name: "nothing requested",
		want: map[kind]string{},
		err:  "needs -toolchain and/or -min",
	}, {
		name: "malformed",
		want: map[kind]string{toolchain: "go1.27.4"},
		err:  "invalid version",
	}, {
		name: "not released",
		want: map[kind]string{toolchain: "1.27.99"},
		err:  "not a Go release",
	}, {
		name: "not stable",
		want: map[kind]string{toolchain: "1.27.5"},
		err:  "not a stable release",
	}, {
		name: "minimum newer than toolchain",
		want: map[kind]string{minimum: "1.27.4"},
		err:  "minimum 1.27.4 is newer than toolchain 1.27.3",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tree := testTree("1.27.3", "1.26.10")
			_, err := set(io.Discard, tree,
				testReleases("1.27.3", "1.26.10", "1.27.4"),
				tc.want, false, noLeftovers)
			require.ErrorContains(t, err, tc.err)
			require.Equal(t, testTree("1.27.3", "1.26.10"), tree)
		})
	}
}

// TestSetLeftovers checks that a mention of the replaced version that no
// rule covers fails the bump.
func TestSetLeftovers(t *testing.T) {
	t.Parallel()

	var patterns []string
	grep := func(p string) ([]string, error) {
		patterns = append(patterns, p)
		return []string{"docs/new.md:3:install Go 1.27.3 here"}, nil
	}
	tree := testTree("1.27.3", "1.26.10")
	rel := testReleases("1.27.3", "1.26.10", "1.27.4")
	_, err := set(io.Discard, tree, rel,
		map[kind]string{toolchain: "1.27.4"}, false, grep)
	require.ErrorContains(t, err, "check(s) failed")
	require.Len(t, patterns, 1, "same major release: no major scan")
}

// TestLeftoverPatterns checks when the full version and the major release of
// a replaced version are scanned for.
func TestLeftoverPatterns(t *testing.T) {
	t.Parallel()

	// Patch bump: only the full version is gone.
	require.Len(t, leftoverPatterns("1.27.3", []string{"1.27.4"}), 1)

	// Major bump: the old major release is gone too.
	require.Len(t, leftoverPatterns("1.26.6", []string{"1.27.1",
		"1.26.8"}), 1, "the minimum keeps 1.26 alive")
	require.Len(t, leftoverPatterns("1.26.6", []string{"1.27.1"}), 2)

	// The toolchain moves away from a version the minimum still uses.
	require.Empty(t, leftoverPatterns("1.27.3", []string{"1.27.4",
		"1.27.3"}))
}

// TestNewer checks the version ordering.
func TestNewer(t *testing.T) {
	t.Parallel()

	require.True(t, newer("1.27.10", "1.27.9"))
	require.True(t, newer("1.28", "1.27.9"))
	require.False(t, newer("1.27.3", "1.27.3"))
	require.False(t, newer("1.26.10", "1.27.3"))
}
