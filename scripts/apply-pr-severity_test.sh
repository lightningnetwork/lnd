#!/usr/bin/env bash
#
# Tests for scripts/apply-pr-severity.sh: the sanitize_comment() filter and the
# main() control flow (label reconciliation and comment gating), the latter with
# gh stubbed via a PATH shim that records its argument lists.
#
# Run: bash scripts/apply-pr-severity_test.sh

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/apply-pr-severity.sh
source "$DIR/apply-pr-severity.sh"

ZWNJ="$(printf '\xe2\x80\x8c')"
fail=0

want()   { grep -qF -- "$2" <<< "$OUT"   && echo "ok:   $1" || { echo "FAIL: $1"; fail=1; }; }
absent() { grep -qF -- "$2" <<< "$OUT"   && { echo "FAIL: $1"; fail=1; } || echo "ok:   $1"; }
eq()     { [[ "$2" == "$3" ]]            && echo "ok:   $1" || { echo "FAIL: $1 (got '$2' want '$3')"; fail=1; }; }
has()    { grep -qF -- "$2" "$GH_CALLS"  && echo "ok:   $1" || { echo "FAIL: $1 (no gh call: $2)"; fail=1; }; }
lacks()  { grep -qF -- "$2" "$GH_CALLS"  && { echo "FAIL: $1 (unexpected gh call: $2)"; fail=1; } || echo "ok:   $1"; }
in_file(){ grep -qF -- "$3" "$2"         && echo "ok:   $1" || { echo "FAIL: $1"; fail=1; }; }

echo "# sanitize_comment"
INPUT='## 🟢 PR Severity: **LOW**
### Analysis
Ping @user, see #123 and org/repo#4567, [x](https://evil), www.evil.net, http://Bad.io.
<!-- pr-severity-bot -->'
OUT="$(printf '%s' "$INPUT" | sanitize_comment)"
want   "## heading preserved"              '## 🟢 PR Severity'
want   "### heading preserved"             '### Analysis'
want   "@-mention defanged"                "@${ZWNJ}user"
want   "#123 defanged"                     "#${ZWNJ}123"
want   "repo#4567 defanged"                "#${ZWNJ}4567"
want   "https scheme defanged"             'hxxp://evil'
want   "http scheme (case-insensitive)"    'hxxp://Bad.io'
want   "www. auto-link defanged"           "www${ZWNJ}.evil.net"
want   "markdown brackets escaped"         '&#91;x&#93;'
want   "bot marker preserved"              '<!-- pr-severity-bot -->'
absent "no live http(s):// scheme"         'https://'
absent "bracket entity not corrupted by #" "&#${ZWNJ}"

echo "# main()"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"
# Fake gh: record the full argument list, and for `pr view` emit the fixture
# labels so main()'s reconciliation has something to diff against.
cat > "$WORK/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "$*" >> "$GH_CALLS"
[[ "$1 $2" == "pr view" ]] && printf '%s' "${GH_FAKE_LABELS:-}"
exit 0
SH
chmod +x "$WORK/bin/gh"
export PATH="$WORK/bin:$PATH"
export GH_TOKEN=x PR_NUMBER=42 REPO=owner/repo

# run_main RESULT_DIR: run main() capturing its return code in RC, with a fresh
# GH_CALLS log for the case.
run_main() { export GH_CALLS="$WORK/calls"; : > "$GH_CALLS"; RC=0; main "$1" >/dev/null 2>&1 || RC=$?; }

# Invalid severity: reject before any gh mutation.
r="$WORK/invalid"; mkdir -p "$r"; printf 'bogus\n' > "$r/severity.txt"
GH_FAKE_LABELS='' run_main "$r"
eq    "invalid severity returns 1"        "$RC" "1"
lacks "invalid severity: no label edit"   "pr edit"

# No verdict at all: warn and no-op, no gh mutation.
r="$WORK/noverdict"; mkdir -p "$r"
GH_FAKE_LABELS='' run_main "$r"
eq    "missing severity.txt returns 0"    "$RC" "0"
lacks "missing verdict: no label edit"    "pr edit"

# Empty/whitespace-only severity.txt is handled like a missing one (F30).
r="$WORK/emptysev"; mkdir -p "$r"; printf '   \n' > "$r/severity.txt"
GH_FAKE_LABELS='' run_main "$r"
eq    "empty severity.txt returns 0"      "$RC" "0"
lacks "empty severity: no label edit"     "pr edit"

# Valid severity, no comment: reconcile labels in one edit, remove only present.
r="$WORK/label"; mkdir -p "$r"; printf 'high\n' > "$r/severity.txt"; printf 'false\n' > "$r/should_comment.txt"
GH_FAKE_LABELS=$'severity-low\nkeep-me\nseverity-medium' run_main "$r"
eq    "label-only returns 0"              "$RC" "0"
has   "adds target label"                 "--add-label severity-high"
has   "removes present severity-low"      "--remove-label severity-low"
has   "removes present severity-medium"   "--remove-label severity-medium"
lacks "does not touch absent critical"    "severity-critical"
lacks "no comment when should_comment false" "pr comment"

# should_comment true with a real body: sanitize and post it.
r="$WORK/comment"; mkdir -p "$r"; printf 'low\n' > "$r/severity.txt"; printf 'true\n' > "$r/should_comment.txt"
printf '## x\nhi @bob\n' > "$r/comment.md"
GH_FAKE_LABELS='' run_main "$r"
eq      "comment path returns 0"          "$RC" "0"
has     "posts sanitized comment"         "pr comment 42 --repo owner/repo --body-file"
in_file "posted body defangs @-mention"   "$r/comment.sanitized.md" "@${ZWNJ}bob"

# Capitalized should_comment is honored, not silently dropped (F31).
r="$WORK/truecase"; mkdir -p "$r"; printf 'low\n' > "$r/severity.txt"; printf 'TRUE\n' > "$r/should_comment.txt"
printf '## x\nhi\n' > "$r/comment.md"
GH_FAKE_LABELS='' run_main "$r"
eq    "should_comment TRUE returns 0"     "$RC" "0"
has   "TRUE still posts the comment"      "pr comment 42 --repo owner/repo --body-file"

# should_comment true but empty body (F15): no post.
r="$WORK/empty"; mkdir -p "$r"; printf 'low\n' > "$r/severity.txt"; printf 'true\n' > "$r/should_comment.txt"
: > "$r/comment.md"
GH_FAKE_LABELS='' run_main "$r"
eq    "empty comment returns 0"           "$RC" "0"
lacks "empty comment: no post"            "pr comment"

# Oversized after sanitization (F22): brackets expand ~5x past the cap; no post.
r="$WORK/big"; mkdir -p "$r"; printf 'low\n' > "$r/severity.txt"; printf 'true\n' > "$r/should_comment.txt"
{ printf '## x\n'; head -c 20000 /dev/zero | tr '\0' '['; } > "$r/comment.md"
GH_FAKE_LABELS='' run_main "$r"
eq    "oversized comment returns 0"       "$RC" "0"
lacks "oversized comment: no post"        "pr comment"

if [[ "$fail" -ne 0 ]]; then
  echo "TESTS FAILED"
  exit 1
fi
echo "ALL TESTS PASSED"
