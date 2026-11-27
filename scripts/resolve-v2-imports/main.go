package main

import (
	"errors"
	"fmt"
	"go/format"
	"os"
	"regexp"
	"strings"
)

var (
	// conflictBlock matches one git conflict block: the HEAD side, then the
	// incoming side.
	conflictBlock = regexp.MustCompile(
		`(?s)<<<<<<< [^\n]*\n(.*?)=======\n(.*?)>>>>>>> [^\n]*\n`,
	)

	// v2Import matches the btcd v2 module imports that master uses and the
	// release branches don't. btcec/v2 is not one of them: it is the same
	// module on every branch.
	v2Import = regexp.MustCompile(`"github\.com/btcsuite/btcd/` +
		`(btcutil|chainhash|wire|txscript|chaincfg|btcutil/psbt|` +
		`address)/v2[^"]*"`)

	// importLine matches an import spec, optionally named, or a blank
	// line.
	importLine = regexp.MustCompile(`^\s*(\w+\s+)?"[^"]+"\s*$|^\s*$`)

	// errNotImports is returned for a conflict that contains anything but
	// imports.
	errNotImports = errors.New("conflict contains a non-import line")
)

// resolve resolves the import-block conflicts in a Go source file. For each
// conflict it keeps the HEAD (release branch) side and appends the incoming
// imports that aren't btcd v2 module paths, i.e. the imports the
// cherry-picked commit really adds. The result is gofmt'ed, which sorts the
// import block. It refuses a file if any conflict contains a non-import line.
func resolve(src []byte) ([]byte, int, error) {
	blocks := conflictBlock.FindAllSubmatch(src, -1)
	for _, b := range blocks {
		lines := strings.Split(string(b[1])+string(b[2]), "\n")
		for _, l := range lines {
			if !importLine.MatchString(l) {
				return nil, 0, fmt.Errorf("%w: %q",
					errNotImports, l)
			}
		}
	}

	out := conflictBlock.ReplaceAllFunc(src, func(block []byte) []byte {
		m := conflictBlock.FindSubmatch(block)
		head := string(m[1])

		var extra strings.Builder
		for _, l := range strings.SplitAfter(string(m[2]), "\n") {
			if l != "" && !v2Import.MatchString(l) {
				extra.WriteString(l)
			}
		}

		return []byte(head + extra.String())
	})
	if strings.Contains(string(out), "<<<<<<<") ||
		strings.Contains(string(out), ">>>>>>>") {

		return nil, 0, errors.New("unresolved conflict markers remain")
	}

	formatted, err := format.Source(out)
	if err != nil {
		return nil, 0, fmt.Errorf("gofmt: %w", err)
	}

	return formatted, len(blocks), nil
}

// main resolves backport conflicts that only exist because master imports the
// btcd v2 modules (btcutil/v2, chainhash/v2, wire/v2, ...) while the release
// branches import v1. It is run on the conflicted files:
//
//	go run ./scripts/resolve-v2-imports <file>...
//
// Use it only for conflicts that sit entirely inside an import block. A file
// whose conflict contains code is left untouched and reported, so it gets
// resolved by hand against the parent commit. The files must be inside the
// working directory, as git status lists them.
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run resolves every file and reports each failure. It fails if any file
// couldn't be resolved.
func run(paths []string) error {
	if len(paths) == 0 {
		return errors.New("usage: resolve-v2-imports <file>...")
	}

	root, err := os.OpenRoot(".")
	if err != nil {
		return err
	}
	defer root.Close()

	failed := 0
	for _, path := range paths {
		if err := resolveFile(root, path); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d file(s) not resolved", failed)
	}

	return nil
}

// resolveFile resolves one file. Opening it through root keeps it inside the
// working directory.
func resolveFile(root *os.Root, path string) error {
	info, err := root.Stat(path)
	if err != nil {
		return err
	}
	src, err := root.ReadFile(path)
	if err != nil {
		return err
	}
	out, n, err := resolve(src)
	if err != nil {
		return err
	}
	if err := root.WriteFile(path, out, info.Mode()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "%s: resolved %d import conflict(s)\n", path,
		n)

	return nil
}
