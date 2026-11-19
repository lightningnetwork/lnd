package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// kind is one of the two Go versions lnd pins.
type kind string

const (
	// toolchain is the Go release used to build lnd: Makefile GO_VERSION,
	// CI workflows, Docker images, the linter config and the build docs.
	toolchain kind = "toolchain"

	// minimum is the Go language version in every go.mod, which is the
	// oldest Go that can build lnd, plus docs/INSTALL.md, which tells users
	// to install it and lists the official archive checksums.
	minimum kind = "min"

	installDoc = "docs/INSTALL.md"
	goDLJSON   = "https://go.dev/dl/?mode=json&include=all"
)

// anchors are the pins that define the current version of each kind. Only
// pins equal to the anchor are rewritten.
var anchors = map[kind]string{toolchain: "Makefile", minimum: "go.mod"}

// rule finds pins of one kind in the files matching its globs. The pattern
// has exactly one capture group holding the version. A major rule pins only
// the major release (1.27) and follows the full version of its kind.
type rule struct {
	kind    kind
	globs   []string
	pattern *regexp.Regexp
	major   bool
}

const (
	full = `(\d+\.\d+(?:\.\d+)?)`
	maj  = `(\d+\.\d+)`
)

func newRule(k kind, globs []string, pattern string, major bool) rule {
	return rule{
		kind:    k,
		globs:   globs,
		pattern: regexp.MustCompile("(?m)" + pattern),
		major:   major,
	}
}

// rules lists every Go version pin in lnd. A rule whose files exist but that
// matches nothing makes the check fail, so a changed file format can't make
// a pin silently go stale.
var rules = []rule{
	newRule(toolchain, []string{"Makefile"},
		`^GO_VERSION = `+full+`$`, false),
	newRule(toolchain, []string{
		".github/workflows/*.yml", ".github/workflows/*.yaml",
	}, `^\s*GO_VERSION: `+full+`\s*$`, false),
	newRule(toolchain, []string{".golangci.yml"},
		`^\s*go: "`+full+`"`, false),
	newRule(toolchain, []string{
		"Dockerfile", "*/Dockerfile", "*/*/Dockerfile",
		"*.Dockerfile", "*/*.Dockerfile",
	}, `^FROM golang:`+full+`(?:[-\s]|$)`, false),
	newRule(toolchain, []string{"lnrpc/gen_protos_docker.sh"},
		`golang:`+full+`(?:[-\s"]|$)`, false),
	newRule(toolchain, []string{".github/actions/setup-go/action.yml"},
		`go-`+full+`-unit-test`, false),
	newRule(toolchain, []string{".github/actions/setup-go/action.yml"},
		`with Go `+maj+` only`, true),
	newRule(toolchain, []string{"mobile/README.md"},
		`(?:golang\.org/dl|dl\.google\.com/go)/go`+full+`\.linux`,
		false),
	newRule(toolchain, []string{"docs/ci-runs.md"},
		`install Go `+full+`\b`, false),
	newRule(minimum, []string{"go.mod", "*/go.mod", "*/*/go.mod"},
		`^go `+full+`$`, false),
	newRule(minimum, []string{installDoc},
		"minimum version of `"+full+"`", false),
	newRule(minimum, []string{installDoc},
		`go`+full+`\.linux-[a-z0-9]+\.tar\.gz`, false),
}

// scanExclude lists paths that mention Go versions for other reasons and are
// never pins.
var scanExclude = []string{
	"*go.sum", "*.go", "docs/release-notes/*", ".claude/*", ".agents/*",
}

var (
	// hex64 matches any SHA256. INSTALL.md checksums are identified by
	// their value on go.dev rather than by line format, since the format
	// has changed before (7fcbbd44ca).
	hex64 = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

	archiveName = regexp.MustCompile(`^go(\d+\.\d+(?:\.\d+)?)\.(.+)$`)
	versionRe   = regexp.MustCompile(`^\d+\.\d+(?:\.\d+)?$`)
)

// tree is the set of tracked files the tool reads and rewrites.
type tree interface {
	files() []string
	read(path string) (string, error)
	write(path, content string) error
}

// pin is one version found by a rule.
type pin struct {
	kind    kind
	path    string
	line    int
	version string
	rule    int
	major   bool
}

// archive is one official Go download.
type archive struct {
	version string
	suffix  string
}

// releases indexes the go.dev release feed.
type releases struct {
	stable   map[string]bool
	sums     map[string]string
	archives map[string]archive
}

func majorOf(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return v
	}

	return parts[0] + "." + parts[1]
}

func parseVersion(v string) []int {
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}

	return out
}

// newer reports whether version a is newer than version b.
func newer(a, b string) bool {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			return x > y
		}
	}

	return false
}

func matchGlob(p string, globs []string) bool {
	for _, g := range globs {
		// path.Match's * never crosses '/', so a glob also fixes the
		// directory depth.
		if ok, _ := path.Match(g, p); ok {
			return true
		}
	}

	return false
}

// findPins returns every pin in the tree, ordered by rule.
func findPins(t tree) ([]pin, error) {
	var pins []pin
	for i, r := range rules {
		for _, p := range t.files() {
			if !matchGlob(p, r.globs) {
				continue
			}
			content, err := t.read(p)
			if err != nil {
				return nil, err
			}
			lines := strings.Split(content, "\n")
			for n, line := range lines {
				ms := r.pattern.FindAllStringSubmatch(line, -1)
				for _, m := range ms {
					pins = append(pins, pin{
						kind: r.kind, path: p,
						line: n + 1, version: m[1],
						rule: i, major: r.major,
					})
				}
			}
		}
	}

	return pins, nil
}

func anchorOf(pins []pin, k kind) string {
	for _, p := range pins {
		if p.kind == k && p.path == anchors[k] && !p.major {
			return p.version
		}
	}

	return ""
}

// report collects the result of a check.
type report struct {
	lines    []string
	failures []string
	current  map[kind]string
}

func (r *report) ok() bool {
	return len(r.failures) == 0
}

func (r *report) failf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// check validates every pin against its anchor and every INSTALL.md checksum
// against go.dev.
func check(t tree, rel *releases) (*report, error) {
	pins, err := findPins(t)
	if err != nil {
		return nil, err
	}
	rep := &report{current: map[kind]string{}}

	for i, r := range rules {
		exists, matched := false, false
		for _, f := range t.files() {
			exists = exists || matchGlob(f, r.globs)
		}
		for _, p := range pins {
			matched = matched || p.rule == i
		}

		// Older trees may lack a file entirely, e.g. docs/ci-runs.md.
		// A file that exists but no longer matches is a format change.
		if exists && !matched {
			rep.failf("rule %d (%s, %v) matched nothing; "+
				"update the rules in scripts/bump-go", i,
				r.kind, r.globs)
		}
	}

	for _, k := range []kind{toolchain, minimum} {
		cur := anchorOf(pins, k)
		rep.current[k] = cur
		rep.lines = append(rep.lines,
			fmt.Sprintf("%s: %s (anchor %s)", k, cur, anchors[k]))
		if cur == "" {
			rep.failf("no %s anchor in %s", k, anchors[k])
			continue
		}
		for _, p := range pins {
			if p.kind != k {
				continue
			}
			want := cur
			if p.major {
				want = majorOf(cur)
			}
			rep.lines = append(rep.lines, fmt.Sprintf("  %s:%d  %s",
				p.path, p.line, p.version))
			if p.version != want {
				rep.failf("%s:%d has %s, %s is %s", p.path,
					p.line, p.version, k, want)
			}
		}
	}

	doc, err := t.read(installDoc)
	if err != nil {
		return nil, err
	}
	for n, line := range strings.Split(doc, "\n") {
		for _, h := range hex64.FindAllString(line, -1) {
			a, known := rel.archives[h]
			switch {
			case !known:
				rep.failf("%s:%d %s... is not a go.dev "+
					"archive checksum", installDoc, n+1,
					h[:12])

			case a.version != rep.current[minimum]:
				rep.failf("%s:%d checksum is for go%s.%s, "+
					"min is %s", installDoc, n+1,
					a.version, a.suffix,
					rep.current[minimum])

			default:
				rep.lines = append(rep.lines, fmt.Sprintf(
					"  checksum ok: go%s.%s", a.version,
					a.suffix))
			}
		}
	}

	return rep, nil
}

// rewriteFile replaces the pins of one kind that hold the old version. It
// returns the new content and the pins it left alone because they hold
// another version.
func rewriteFile(p, content string, k kind, oldV, newV string) (string,
	[]string) {

	var skipped []string
	for _, r := range rules {
		if r.kind != k || !matchGlob(p, r.globs) {
			continue
		}
		from, to := oldV, newV
		if r.major {
			from, to = majorOf(oldV), majorOf(newV)
		}
		content = replaceGroup(r.pattern, content, func(v,
			match string) string {

			if v != from {
				skipped = append(skipped, fmt.Sprintf(
					"%s: %s", p, strings.TrimSpace(match),
				))

				return v
			}

			return to
		})
	}

	return content, skipped
}

// replaceGroup replaces capture group 1 of every match of re in s with the
// result of fn, keeping the rest of the match.
func replaceGroup(re *regexp.Regexp, s string,
	fn func(group, match string) string) string {

	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:loc[2]])
		b.WriteString(fn(s[loc[2]:loc[3]], s[loc[0]:loc[1]]))
		last = loc[3]
	}
	b.WriteString(s[last:])

	return b.String()
}

// rewriteChecksums swaps every go.dev checksum of an archive of oldV for the
// checksum of the same archive of newV.
func rewriteChecksums(content, oldV, newV string, rel *releases) (string,
	error) {

	var err error
	out := hex64.ReplaceAllStringFunc(content, func(h string) string {
		a, ok := rel.archives[h]
		if !ok || a.version != oldV {
			return h
		}
		name := "go" + newV + "." + a.suffix
		sum, ok := rel.sums[name]
		if !ok {
			err = fmt.Errorf("no go.dev checksum for %s", name)
			return h
		}

		return sum
	})

	return out, err
}

// bump rewrites the pins of one kind from oldV to newV and returns the
// changed files and the pins left alone.
func bump(t tree, k kind, oldV, newV string, rel *releases) ([]string,
	[]string, error) {

	var changed, skipped []string
	for _, p := range t.files() {
		src, err := t.read(p)
		if err != nil {
			return nil, nil, err
		}
		out, s := rewriteFile(p, src, k, oldV, newV)
		skipped = append(skipped, s...)
		if k == minimum && p == installDoc {
			out, err = rewriteChecksums(out, oldV, newV, rel)
			if err != nil {
				return nil, nil, err
			}
		}
		if out == src {
			continue
		}
		if err := t.write(p, out); err != nil {
			return nil, nil, err
		}
		changed = append(changed, p)
	}

	return changed, skipped, nil
}

// leftoverPatterns returns the extended regexps that find mentions of a
// replaced version. On a major bump, they also find mentions of the old major
// release next to a Go keyword.
func leftoverPatterns(oldV string, keep []string) []string {
	var out []string
	keepFull, keepMajor := false, false
	for _, k := range keep {
		keepFull = keepFull || k == oldV
		keepMajor = keepMajor || majorOf(k) == majorOf(oldV)
	}
	if !keepFull {
		out = append(out, `(^|[^0-9.])`+regexp.QuoteMeta(oldV)+
			`([^0-9]|$)`)
	}
	if !keepMajor {
		out = append(out, `([Gg]o|golang)[ -]?`+
			regexp.QuoteMeta(majorOf(oldV))+`([^.0-9]|$)`)
	}

	return out
}

// gitGrep returns the tracked lines matching an extended regexp, outside the
// excluded paths.
func gitGrep(pattern string) ([]string, error) {
	args := []string{"grep", "-nE", pattern, "--", "."}
	for _, g := range scanExclude {
		args = append(args, ":!"+g)
	}
	out, err := exec.Command("git", args...).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// fetchReleases downloads and indexes the go.dev release feed.
func fetchReleases() (*releases, error) {
	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(goDLJSON)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var feed []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
		Files   []struct {
			Filename string `json:"filename"`
			SHA256   string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&feed); err != nil {
		return nil, fmt.Errorf("decode %s: %w", goDLJSON, err)
	}

	rel := &releases{
		stable:   map[string]bool{},
		sums:     map[string]string{},
		archives: map[string]archive{},
	}
	for _, r := range feed {
		rel.stable[strings.TrimPrefix(r.Version, "go")] = r.Stable
		for _, f := range r.Files {
			rel.sums[f.Filename] = f.SHA256
			m := archiveName.FindStringSubmatch(f.Filename)
			if m != nil && f.SHA256 != "" {
				rel.archives[f.SHA256] = archive{m[1], m[2]}
			}
		}
	}

	return rel, nil
}

// diskTree is the tree of files tracked by git in the working directory.
type diskTree struct {
	tracked []string
}

func (d *diskTree) files() []string {
	return d.tracked
}

func (d *diskTree) read(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}

func (d *diskTree) write(p, content string) error {
	info, err := os.Stat(p)
	if err != nil {
		return err
	}

	return os.WriteFile(p, []byte(content), info.Mode())
}

func trackedFiles() ([]string, error) {
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// main checks or bumps the Go versions lnd pins. It is run from the
// repository root:
//
//	go run ./scripts/bump-go status
//	go run ./scripts/bump-go set [-toolchain X.Y.Z] [-min X.Y.Z]
//	    [-allow-inconsistent]
//
// status prints every pin and fails if a pin disagrees with its anchor
// (Makefile GO_VERSION for the toolchain, the root go.mod for the minimum),
// if an INSTALL.md hash isn't the go.dev checksum of a minimum-version
// archive, or if a pinned file no longer matches its rule.
//
// set rewrites the pins that hold the current version, swaps the INSTALL.md
// checksums for the new archives, checks the result like status, and fails if
// the replaced version is still mentioned anywhere it isn't excluded. It
// refuses to start on an inconsistent tree unless -allow-inconsistent is
// given, in which case pins on other versions are left alone and reported.
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || (args[0] != "status" && args[0] != "set") {
		return errors.New("usage: bump-go status | bump-go set " +
			"[-toolchain X.Y.Z] [-min X.Y.Z] [-allow-inconsistent]")
	}

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	newToolchain := fs.String("toolchain", "", "new toolchain version")
	newMin := fs.String("min", "", "new minimum version")
	allow := fs.Bool("allow-inconsistent", false,
		"bump even if the pins already disagree")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	files, err := trackedFiles()
	if err != nil {
		return err
	}
	t := &diskTree{tracked: files}
	rel, err := fetchReleases()
	if err != nil {
		return err
	}

	if args[0] == "status" {
		rep, err := check(t, rel)
		if err != nil {
			return err
		}

		return printReport(os.Stdout, rep)
	}

	want := map[kind]string{toolchain: *newToolchain, minimum: *newMin}
	changed, err := set(os.Stdout, t, rel, want, *allow, gitGrep)
	for _, p := range changed {
		fmt.Fprintln(os.Stdout, "changed:", p)
	}

	return err
}

// set bumps the requested versions and verifies the result, writing its
// report to out. grep finds leftover mentions of the replaced versions.
func set(out io.Writer, t tree, rel *releases, want map[kind]string,
	allow bool, grep func(string) ([]string, error)) ([]string, error) {

	if want[toolchain] == "" && want[minimum] == "" {
		return nil, errors.New("set needs -toolchain and/or -min")
	}

	before, err := check(t, rel)
	if err != nil {
		return nil, err
	}
	if !before.ok() && !allow {
		for _, f := range before.failures {
			fmt.Fprintln(out, "FAIL", f)
		}

		return nil, errors.New("refusing to bump: the pins are " +
			"already inconsistent; fix them first or pass " +
			"-allow-inconsistent")
	}

	target := map[kind]string{}
	for _, k := range []kind{toolchain, minimum} {
		target[k] = before.current[k]
		if want[k] == "" {
			continue
		}
		if !versionRe.MatchString(want[k]) {
			return nil, fmt.Errorf("invalid version %q", want[k])
		}
		stable, listed := rel.stable[want[k]]
		if !listed {
			return nil, fmt.Errorf("go%s is not a Go release "+
				"listed on go.dev", want[k])
		}
		if !stable {
			return nil, fmt.Errorf("go%s is not a stable release",
				want[k])
		}
		if before.current[k] == "" {
			return nil, fmt.Errorf("no %s anchor in %s", k,
				anchors[k])
		}
		target[k] = want[k]
	}
	if newer(target[minimum], target[toolchain]) {
		return nil, fmt.Errorf("minimum %s is newer than toolchain %s",
			target[minimum], target[toolchain])
	}
	tm, mm := parseVersion(target[toolchain]), parseVersion(target[minimum])
	if tm[0] == mm[0] && tm[1]-mm[1] > 1 {
		fmt.Fprintf(out, "WARN minimum %s is more than one major "+
			"release behind toolchain %s; lnd supports the last "+
			"two Go releases\n", target[minimum],
			target[toolchain])
	}

	var changed []string
	for _, k := range []kind{toolchain, minimum} {
		if target[k] == before.current[k] {
			continue
		}
		c, skipped, err := bump(t, k, before.current[k], target[k], rel)
		if err != nil {
			return changed, err
		}
		changed = append(changed, c...)
		for _, s := range skipped {
			fmt.Fprintf(out, "WARN left alone (not on %s): %s\n",
				before.current[k], s)
		}
	}
	sort.Strings(changed)
	changed = dedupe(changed)

	after, err := check(t, rel)
	if err != nil {
		return changed, err
	}
	for _, k := range []kind{toolchain, minimum} {
		if after.current[k] != target[k] {
			after.failf("%s is %s, wanted %s", k, after.current[k],
				target[k])
		}
	}

	keep := []string{target[toolchain], target[minimum]}
	for _, k := range []kind{toolchain, minimum} {
		if target[k] == before.current[k] {
			continue
		}
		old := before.current[k]
		for _, pattern := range leftoverPatterns(old, keep) {
			lines, err := grep(pattern)
			if err != nil {
				return changed, err
			}
			for _, l := range lines {
				after.failf("leftover %s: %s", old, l)
			}
		}
	}

	return changed, printReport(out, after)
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}

	return out
}

func printReport(out io.Writer, rep *report) error {
	for _, l := range rep.lines {
		fmt.Fprintln(out, l)
	}
	for _, f := range rep.failures {
		fmt.Fprintln(out, "FAIL", f)
	}
	if !rep.ok() {
		return fmt.Errorf("%d check(s) failed", len(rep.failures))
	}

	return nil
}
