#!/bin/bash

# Exercise the release workflow's actual preparation step with local Git and
# fixture CLI output, so checks cannot create public tags or release assets.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Arrange the workflow contract check by loading YAML with Ruby's standard
# library, which also lets the execution cases reuse the actual shell block.
ruby -ryaml - "$DIR/../.github/workflows/release.yaml" "$WORK/prepare.sh" <<'RUBY'
workflow = YAML.load_file(ARGV.fetch(0))
# YAML 1.1 treats the Actions key "on" as true; accept either parser spelling.
events = workflow['on'] || workflow[true]
steps = workflow.fetch('jobs').fetch('main').fetch('steps')

# Act by reading the public trigger and draft settings rather than matching
# implementation text inside the workflow's shell commands.
push = events.fetch('push')
release = steps.find { |step| step['name'] == 'Create Release' }.fetch('with')
version = steps.find { |step| step['id'] == 'version' }

# Assert that version-file pushes prepare drafts at an exact commit and that
# pushing the human-signed tag cannot rerun this release build.
raise 'release branch filter' unless push['branches'] == ['v0.*-branch']
raise 'version path filter' unless push['paths'] == ['build/version.go']
raise 'unexpected tag trigger' if push.key?('tags')
raise 'manual retry unavailable' unless events.key?('workflow_dispatch')
raise 'release must stay draft' unless release['draft'] == true
unless release['tag_name'] == '${{ env.RELEASE_VERSION }}'
  raise 'prospective tag missing'
end
unless release['target_commitish'] == '${{ github.sha }}'
  raise 'tested commit missing'
end
File.write(ARGV.fetch(1), version.fetch('run'))
RUBY

# Supply only the established lnd version output; Git operations remain real.
mkdir "$WORK/bin"
cat > "$WORK/bin/go" <<'SH'
#!/bin/bash
# Reject other calls so the fixture cannot hide a different build operation.
[[ "$*" == 'run ./cmd/lnd --version' ]] || exit 1
printf 'lnd version %s commit=fixture\n' "$TEST_VERSION"
SH
chmod +x "$WORK/bin/go"

# new_repo isolates each case with a committed HEAD and a local bare origin.
# Disable commit signing only in the fixture so tests never use a human key.
new_repo() {
    REPO="$WORK/$1"
    git init --quiet "$REPO"
    git -C "$REPO" -c user.name=Fixture -c user.email=fixture@example.com \
        -c commit.gpgSign=false commit --quiet --allow-empty -m fixture
    git init --quiet --bare "$REPO.git"
    git -C "$REPO" remote add origin "$REPO.git"
    : > "$REPO/env"
}

# Arrange a new release and a signing preference that CI must override without
# accessing a private key. Its origin initially has no published tag.
new_repo release
git -C "$REPO" config tag.gpgSign true

# Act by executing the workflow's preparation block in the fixture checkout,
# supplying the same environment-file interface used by the Actions runner.
(
    cd "$REPO"
    PATH="$WORK/bin:$PATH" TEST_VERSION=0.21.9-beta GITHUB_ENV="$REPO/env" \
        bash -eo pipefail "$WORK/prepare.sh"
)

# Assert that CI exports the derived version and creates only an unsigned
# local tag at the tested commit; nothing reaches even the local bare origin.
grep -qx 'RELEASE_VERSION=v0.21.9-beta' "$REPO/env"
test "$(git -C "$REPO" rev-parse refs/tags/v0.21.9-beta)" = \
    "$(git -C "$REPO" rev-parse HEAD)"
test "$(git -C "$REPO" cat-file -t refs/tags/v0.21.9-beta)" = commit
if git --git-dir="$REPO.git" show-ref --verify --quiet \
    refs/tags/v0.21.9-beta; then
    echo 'FAIL: preparation published a tag' >&2
    exit 1
fi
echo 'ok: new release prepares only a local unsigned tag'

# Arrange the documented development version in a fresh repository, so no
# release from another case can accidentally make the skip assertions pass.
new_repo development

# Act through the same workflow block with the development CLI version.
(
    cd "$REPO"
    PATH="$WORK/bin:$PATH" TEST_VERSION=0.21.99-beta GITHUB_ENV="$REPO/env" \
        bash -eo pipefail "$WORK/prepare.sh"
)

# Assert that no release version is exported and no local tag is created,
# leaving the later notes, build, and upload steps without a release to run.
test ! -s "$REPO/env"
test -z "$(git -C "$REPO" tag --list)"
echo 'ok: development version skips release preparation'

# Arrange an already present release tag and remember its object so a retry
# must preserve it rather than replacing it with the current checkout.
new_repo tagged
git -C "$REPO" -c tag.gpgSign=false tag v0.21.9-beta
TAG_OBJECT="$(git -C "$REPO" rev-parse refs/tags/v0.21.9-beta)"

# Act by requesting preparation of that same version through the workflow.
(
    cd "$REPO"
    PATH="$WORK/bin:$PATH" TEST_VERSION=0.21.9-beta GITHUB_ENV="$REPO/env" \
        bash -eo pipefail "$WORK/prepare.sh"
)

# Assert that already tagged releases export no preparation version and keep
# the original object, preventing a retry from replacing release assets.
test ! -s "$REPO/env"
test "$(git -C "$REPO" rev-parse refs/tags/v0.21.9-beta)" = "$TAG_OBJECT"
echo 'ok: already tagged release skips preparation'
