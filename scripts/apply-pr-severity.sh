#!/usr/bin/env bash
#
# Applies the PR severity label and posts the classifier's comment.
#
# Reads the classifier's verdict from a result directory (arg $1, default
# "result") and, in this order:
#   - validates the severity against the known set,
#   - reconciles the severity-* label in a single gh edit, and
#   - sanitizes and posts the model-authored comment.
#
# The classify job runs a model on untrusted PR text, so the comment body is
# treated as tainted and passed through sanitize_comment() before posting.
#
# Env: GH_TOKEN (gh auth), PR_NUMBER, REPO.
# Usage: ./apply-pr-severity.sh [RESULT_DIR]

set -euo pipefail

# sanitize_comment reads an untrusted comment body on stdin and writes a safe
# version to stdout. GitHub already strips scripts and unsafe HTML from rendered
# comment bodies, so this targets the notification/spam class: @-mentions that
# ping arbitrary users, `#N` cross-references that notify other issues/PRs,
# auto-linked URLs used for phishing, and Markdown links. It deliberately leaves
# the model's <details>/<sub> markup intact so the comment still renders.
#
# The zero-width non-joiner is spliced in as a literal byte (not a `\xNN` sed
# escape) so the rules work identically under both GNU and BSD sed. Rule order
# matters: the `#`-before-digit defang runs before the bracket rules, which
# themselves emit `&#91;`/`&#93;` — running it after would corrupt those
# entities. Matching only `#` followed by a digit leaves `##`/`###` Markdown
# headings untouched.
sanitize_comment() {
  local z
  z="$(printf '\xe2\x80\x8c')"
  sed -E \
    -e "s/@/@${z}/g" \
    -e "s/www\./www${z}./g" \
    -e 's,[hH][tT][tT][pP][sS]?://,hxxp://,g' \
    -e "s/#([0-9])/#${z}\1/g" \
    -e 's/\[/\&#91;/g' \
    -e 's/\]/\&#93;/g'
}

main() {
  local result_dir="${1:-result}"
  : "${GH_TOKEN:?GH_TOKEN is required}"
  : "${PR_NUMBER:?PR_NUMBER is required}"
  : "${REPO:?REPO is required}"

  # Treat a missing severity.txt and an empty/whitespace-only one the same way:
  # both mean the classify job produced no usable verdict (model crash or
  # timeout, a missing artifact tolerated by the download step, or a truncated
  # write). Warn and degrade rather than laundering a broken run into a silent
  # green or failing the check red, since severity classification is advisory.
  # A non-empty but unrecognized value still fails below.
  #
  # Lowercase on read so a stray `Low`/`HIGH` from the model still matches.
  local severity=""
  if [[ -f "$result_dir/severity.txt" ]]; then
    severity="$(tr -d '[:space:]' < "$result_dir/severity.txt" | tr '[:upper:]' '[:lower:]')"
  fi
  if [[ -z "$severity" ]]; then
    echo "::warning::PR severity classifier produced no verdict; PR left unlabeled."
    return 0
  fi

  # Strictly validate the severity against the known set — it is the only
  # privileged, semantically meaningful output.
  case "$severity" in
    critical|high|medium|low) ;;
    *)
      echo "Invalid severity '$severity'; refusing to apply." >&2
      return 1
      ;;
  esac

  # Read the PR's current severity-* labels so the reconciliation below removes
  # exactly the ones present. (The severity-change banner is authored by the
  # model in comment.md, so no previous severity is needed here.)
  #
  # Fail closed on a read error (rate limit, 5xx): swallowing it would yield
  # empty labels, so the loop below would remove nothing yet still add the new
  # label — the exact two-label state this single-edit design prevents. With
  # set -e, a failed read aborts before any edit. (local is declared separately
  # so the assignment's own exit status, not local's, drives set -e.)
  local cur_labels
  cur_labels="$(gh pr view "$PR_NUMBER" --repo "$REPO" \
    --json labels --jq '.labels[].name')"

  # Reconcile the severity label in a SINGLE gh edit: add the target and remove
  # exactly the other severity-* labels currently present. Doing it in one call
  # means:
  #   - if the add fails (label missing from the repo, transient API error), the
  #     whole edit fails and the PR keeps its prior label rather than being left
  #     unlabeled; and
  #   - a run cancelled by `concurrency.cancel-in-progress` can't stop between an
  #     add and a separate remove, so the PR is never left carrying two labels.
  # Only present labels are removed, so gh never errors on a missing one.
  local edit_args level
  edit_args=(--add-label "severity-$severity")
  for level in critical high medium low; do
    [[ "$level" == "$severity" ]] && continue
    if grep -qx "severity-$level" <<< "$cur_labels"; then
      edit_args+=(--remove-label "severity-$level")
    fi
  done
  gh pr edit "$PR_NUMBER" --repo "$REPO" "${edit_args[@]}"

  # Lowercase on read (as with severity) so a `True`/`TRUE` slip from the model
  # is honored rather than silently dropping the comment.
  local should_comment="false"
  if [[ -f "$result_dir/should_comment.txt" ]]; then
    should_comment="$(tr -d '[:space:]' < "$result_dir/should_comment.txt" | tr '[:upper:]' '[:lower:]')"
  fi

  # Use -s (exists and non-empty): a 0-byte comment.md would otherwise reach
  # `gh pr comment --body-file`, which rejects an empty body and would abort
  # after the label was already applied.
  if [[ "$should_comment" != "true" || ! -s "$result_dir/comment.md" ]]; then
    echo "No comment requested; done."
    return 0
  fi

  # Sanitize first, then enforce the size cap on the SANITIZED body. Every
  # sanitizer rule only grows the byte count (e.g. `[` -> `&#91;`), so a body
  # that fits before sanitizing can exceed GitHub's comment limit afterward;
  # measuring the post-sanitization size is what actually bounds the request.
  sanitize_comment < "$result_dir/comment.md" > "$result_dir/comment.sanitized.md"

  local max_bytes=16384
  if [[ "$(wc -c < "$result_dir/comment.sanitized.md")" -gt "$max_bytes" ]]; then
    echo "::warning::classifier comment exceeds ${max_bytes} bytes after sanitization; skipping comment."
    return 0
  fi

  # Post from a file (never interpolated into the shell) so the body is handled
  # purely as data.
  gh pr comment "$PR_NUMBER" --repo "$REPO" \
    --body-file "$result_dir/comment.sanitized.md"
}

# Allow sourcing (e.g. from the test) without executing main.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
