#!/usr/bin/env bash

set -euo pipefail

SRC_BRANCH=""
RELEASE_BRANCH=""
SRC_SCAN_LIMIT=1000
RELEASE_LIMIT=0

show_help() {
  echo "Usage: $0 --source <branch> --release <branch> [--scan-limit N] [--limit N]"
  echo ""
  echo "  --source      Branch where cherry-picks originated (e.g. master)"
  echo "  --release     Branch where cherry-picks landed (e.g. release-rc1)"
  echo "  --scan-limit  Max commits to scan in source branch (default: 1000)"
  echo "  --limit       Number of release commits to compare (default: all)"
  exit 1
}

normalize_patch() {
  sed '/^index [0-9a-f]\{7,\}\.\.[0-9a-f]\{7,\} [0-9]\{6\}$/d'
}

# Parse args
while [[ $# -gt 0 ]]; do
  case "$1" in
    --source|--release|--scan-limit|--limit)
      if [[ -z "${2:-}" || "$2" =~ ^- ]]; then
        echo "Error: Missing value for argument $1" >&2
        show_help
      fi
      case "$1" in
        --source) SRC_BRANCH="$2" ;;
        --release) RELEASE_BRANCH="$2" ;;
        --scan-limit) SRC_SCAN_LIMIT="$2" ;;
        --limit) RELEASE_LIMIT="$2" ;;
      esac
      shift 2
      ;;
    -h|--help) show_help ;;
    *) echo "Unknown argument: $1"; show_help ;;
  esac
done

if [[ -z "$SRC_BRANCH" || -z "$RELEASE_BRANCH" ]]; then
  echo "❌ Missing required arguments."; show_help
fi

# Cross-platform hashing
hash_patch() {
  if command -v md5sum >/dev/null 2>&1; then
    md5sum | awk '{print $1}'
  else
    md5 | awk '{print $NF}'
  fi
}

echo "🔍 Preparing comparison:"
echo "    Source  branch : $SRC_BRANCH"
echo "    Release branch : $RELEASE_BRANCH"
echo "    Max source scan: $SRC_SCAN_LIMIT"
echo "    Max release compare: $([[ $RELEASE_LIMIT -gt 0 ]] && echo \"$RELEASE_LIMIT\" || echo \"ALL\")"
echo ""

echo "🔄 Fetching latest refs..."
git fetch --all --quiet || true

echo "📥 Collecting release commits..."
RELEASE_COMMITS=$(git rev-list --no-merges "$RELEASE_BRANCH" ^"$SRC_BRANCH")
if [[ "$RELEASE_LIMIT" -gt 0 ]]; then
  RELEASE_COMMITS=$(echo "$RELEASE_COMMITS" | head -n "$RELEASE_LIMIT")
fi
RELEASE_COMMITS=$(echo "$RELEASE_COMMITS" | awk '{ lines[NR] = $0 } END { for (i = NR; i > 0; i--) print lines[i] }')
RELEASE_COMMITS_ARRAY=()
while IFS= read -r line; do
  [[ -n "$line" ]] && RELEASE_COMMITS_ARRAY+=("$line")
done <<< "$RELEASE_COMMITS"
echo "    → Found ${#RELEASE_COMMITS_ARRAY[@]} release commits."

if [[ "${#RELEASE_COMMITS_ARRAY[@]}" -eq 0 ]]; then
  echo "❌ No release commits found. Exiting."
  exit 1
fi

echo "📥 Collecting source commits..."
SRC_COMMITS=$(git rev-list --no-merges --max-count="$SRC_SCAN_LIMIT" "$SRC_BRANCH")
SRC_COMMITS_ARRAY=()
while IFS= read -r line; do
  [[ -n "$line" ]] && SRC_COMMITS_ARRAY+=("$line")
done <<< "$SRC_COMMITS"
echo "    → Found ${#SRC_COMMITS_ARRAY[@]} source commits to scan."
echo ""

echo "⚙️  Indexing source commit metadata..."
echo "    → Processing ${#SRC_COMMITS_ARRAY[@]} commits from $SRC_BRANCH..."
SRC_COMMIT_META=()
SRC_PATCH_HASHES=()
SRC_PATCHES=()

progress=0
for commit in "${SRC_COMMITS_ARRAY[@]}"; do
  progress=$((progress + 1))
  echo -ne "\r      [$progress/${#SRC_COMMITS_ARRAY[@]}] Indexing $commit"
  author=$(git log -1 --pretty=format:"%an <%ae>" "$commit" 2>/dev/null) || continue
  subject=$(git log -1 --pretty=format:"%s" "$commit" 2>/dev/null) || continue
  authordate=$(git log -1 --pretty=format:"%ai" "$commit" 2>/dev/null) || continue
  meta_key="${subject}__${author}__${authordate}"
  patch=$(git show --format= --unified=3 "$commit" | normalize_patch | sed 's/^[[:space:]]*//')
  patch_hash=$(echo "$patch" | hash_patch)

  SRC_COMMIT_META+=("$meta_key")
  SRC_PATCH_HASHES+=("$patch_hash")
  SRC_PATCHES+=("$patch")
done

echo -e "\n    → Completed source indexing."

TOTAL=${#RELEASE_COMMITS_ARRAY[@]}
MATCHED=0
UNMATCHED=0

for i in "${!RELEASE_COMMITS_ARRAY[@]}"; do
  rc_commit="${RELEASE_COMMITS_ARRAY[$i]}"
  rc_author=$(git log -1 --pretty=format:"%an <%ae>" "$rc_commit" 2>/dev/null) || continue
  rc_subject=$(git log -1 --pretty=format:"%s" "$rc_commit" 2>/dev/null) || continue
  rc_authordate=$(git log -1 --pretty=format:"%ai" "$rc_commit" 2>/dev/null) || continue
  meta_key="${rc_subject}__${rc_author}__${rc_authordate}"

  echo -ne "[$((i + 1))/$TOTAL] Checking ${rc_commit:0:7}... "

  rc_patch=$(git show --format= --unified=3 "$rc_commit" | normalize_patch | sed 's/^[[:space:]]*//')
  rc_patch_hash=$(echo "$rc_patch" | hash_patch)

  found_exact_index=-1
  for j in "${!SRC_PATCH_HASHES[@]}"; do
    if [[ "${SRC_PATCH_HASHES[$j]}" == "$rc_patch_hash" ]]; then
      found_exact_index=$j
      break
    fi
  done

  if [[ $found_exact_index -ne -1 ]]; then
    found_exact="${SRC_COMMITS_ARRAY[$found_exact_index]}"
    meta_info="${SRC_COMMIT_META[$found_exact_index]}"
    src_subject="${meta_info%%__*}"
    rest="${meta_info#*__}"
    src_author="${rest%%__*}"
    src_authordate="${rest##*__}"
    echo "✅ MATCHES ${found_exact:0:7}"
    echo "    ↪ RELEASE: $rc_commit"
    echo "        Author : $rc_author"
    echo "        Date   : $rc_authordate"
    echo "        Subject: \"$rc_subject\""
    echo "    ↪ SOURCE : $found_exact"
    echo "        Author : $src_author"
    echo "        Date   : $src_authordate"
    echo "        Subject: \"$src_subject\""
    echo ""
    MATCHED=$((MATCHED + 1))
    continue
  fi

  echo "❌ NO MATCH"
  UNMATCHED=$((UNMATCHED + 1))

  echo "🔍 Unmatched Commit:"
  echo "    ↪ Commit : $rc_commit"
  echo "    ↪ Author : $rc_author"
  echo "    ↪ Subject: \"$rc_subject\""
  echo ""

  best_score=99999
  best_index=""
  fuzzy_candidates=0

  for j in "${!SRC_COMMIT_META[@]}"; do
    if [[ "${SRC_COMMIT_META[$j]}" == "$meta_key" ]]; then
      ((fuzzy_candidates++))
      diff=$(diff -u <(echo "$rc_patch") <(echo "${SRC_PATCHES[$j]}") || true)
      score=$(echo "$diff" | grep -vE '^(--- |\+\+\+ )' | grep -c '^[-+]')
      if [[ "$score" -lt "$best_score" ]]; then
        best_score=$score
        best_index=$j
      fi
    fi
  done

  if [[ "$fuzzy_candidates" -eq 0 ]]; then
    echo "⚠️ No commits with matching author + subject + date in source branch."
  else
    match_commit="${SRC_COMMITS_ARRAY[$best_index]}"
    match_author=$(git log -1 --pretty=format:"%an <%ae>" "$match_commit")
    match_subject=$(git log -1 --pretty=format:"%s" "$match_commit")

    changed_files=$(git show --pretty="" --name-only "$rc_commit")

    echo "🤔 Closest fuzzy match: $match_commit  ($best_score changed lines from $fuzzy_candidates candidates)"
    echo "    ↪ Author : $match_author"
    echo "    ↪ Subject: \"$match_subject\""
    echo "    ↪ Files Changed:"
    echo "$changed_files" | sed 's/^/        - /'
    echo ""

    echo "🔧 Check it manually (patch diff):"
    echo "    git diff $match_commit $rc_commit -- \$(git show --pretty=\"\" --name-only $rc_commit)"
    echo ""

    echo "🔍 Diff between release and closest match:"
    echo "---------------------------------------------"
    git diff "$match_commit" "$rc_commit" -- $changed_files | sed 's/^/    /' || true
    echo "---------------------------------------------"
    echo ""
  fi

done

# Summary
echo ""
echo "🔎 Summary:"
echo "  ✅ Matched   : $MATCHED"
echo "  ❌ Unmatched : $UNMATCHED"
echo "  📦 Total     : $TOTAL"

