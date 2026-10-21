# Release Notes

Starting with v0.22, release note entries are added as
individual Markdown files, one per pull request, instead of editing a shared
release note document. This avoids merge conflicts caused by every pull
request editing the same file and its contributor list.

The fragments live in `docs/release-notes/unreleased/` and are not tied to a
release. Each fragment is folded into the release note document of the first
release that ships the change: the next major release for changes on `master`,
or a patch release for changes that are backported to a release branch. Thus
`unreleased/` only ever contains changes that have not been released yet.

The approach is based on Bitcoin Core's release note process. Bitcoin Core
[uses a file named for each pull request][bitcoin-developer-notes] and combines
the files before a release; its repository contains [current examples of these
fragments][bitcoin-fragment]. Its release manager then generates the
contributor list from Git history as part of the
[release process][bitcoin-release-process].

## Adding an Entry

Create a file named after the pull request:

```text
docs/release-notes/unreleased/<PR number>.md
```

For example, a pull request numbered `NNNNN` adds `unreleased/NNNNN.md`. Copy
only the applicable heading hierarchy from `release-notes-template.md`,
followed by the entry:

```markdown
# Improvements

## Performance Improvements

* The SQL backend now [caches repeated bucket
  lookups](https://github.com/lightningnetwork/lnd/pull/NNNNN), reducing the
  number of database queries made by the wallet.
```

Follow these rules:

* Use the pull request number as the filename. If one pull request needs
  entries under several headings, put all of them in the same file.
* Include a link to the pull request in every entry.
* Start the file with a top-level heading (`# ...`) and use headings exactly as
  written in `release-notes-template.md`, so the fragments can be combined
  without renaming sections. Sections that only exist in the newest
  version-wide release note document, such as `## BOLT 12 (Offers)` in
  `release-notes-0.22.0.md`, may be used as well.
* Do not add a contributors section. It is generated when the release notes are
  consolidated.
* Do not add entries directly to the version-wide `release-notes-<version>.md`
  files. They are only written when a release is prepared.

Changes that do not need a release note should continue to use the
`no-changelog` label.

The release note CI check enforces these rules. Unless it carries the
`no-changelog` label, a pull request must be linked from a fragment in
`unreleased/`, usually its own, and must not modify the version-wide
documents. Every fragment a pull request adds must be located in
`unreleased/`, be named after the pull request, link to it, start with a
top-level heading and only use known headings without a contributors section.
Fragments that a pull request modifies, e.g. to extend the entry of an earlier
pull request, are only checked for their location and headings.

### Transition

`release-notes-0.22.0.md` and the patch release documents that were still
being written when fragments were introduced, such as
`release-notes-0.21.5.md`, already contain entries. They serve as the baseline
when their release is prepared. No entries are added to them directly anymore;
new changes reach them through fragments.

## Backports

A fragment needs no changes when its pull request is backported. The automated
backport workflow cherry-picks all commits of the pull request, so the fragment
is added to the release branch along with the change, and the backport pull
request is labeled `no-changelog`. A manual backport must include the commit
that adds the fragment as well.

A change that is only made on a release branch adds its fragment to
`unreleased/` on that branch.

## Preparing a Release

Major releases are prepared on `master` before the first release candidate,
patch releases on their release branch. In both cases, fold the fragments into
the release note document of the new version:

```shell
make release-notes version=<major>.<minor>.<patch> [from=<previous-version-tag>]
```

The fold is done by the Go program in `scripts/release-notes`:

1. It starts with `release-notes-<version>.md`, creating it from
   `release-notes-template.md` if it does not exist yet.
2. It deletes the fragments whose pull request is already announced in another
   release note document. For a major release, these are the changes that
   were backported to a patch release of the previous version.
3. It appends the entries of each remaining fragment to the section of the
   document with the same heading path, ordered by pull request number. It
   fails without changing anything if a section does not exist in the
   document.
4. It generates the contributor names from the Git authors of the release's
   commit range, `<from>..HEAD`, and merges them with the names already listed.
   `from` defaults to the newest final release tag of a lower version, e.g.
   `v0.21.5-beta` for `0.22.0`. Versions are compared rather than Git ancestry,
   since the tags of the previous version are on its release branch and not
   reachable from `master`. This follows Bitcoin Core, with merge commits
   excluded since lnd merges pull requests through GitHub. Names that only
   differ in case are listed once, and bots are skipped.

   The list uses the Git author names as reported by `%aN`, rather than GitHub
   handles or names chosen by hand. If one person appears under several names,
   map them to a single name in a `.mailmap` file at the repository root, which
   Git applies to `%aN`, instead of editing the generated list. That way the
   fix also applies to future releases. Names already listed under an alias
   are replaced with the `.mailmap` name on the next fold, while names added
   by hand, such as co-authors, are kept.
5. It deletes the folded fragments, but keeps `unreleased/.gitkeep` so the
   directory stays in place.

Review the result before committing it. Preserve the wording and pull request
links of the entries, but edit ordering and transitions as needed, and check
co-authored commits and the contributor list. The fold can be run again, e.g.
for fragments that were added after a release candidate, without duplicating
the existing entries or contributors. The pull request that finalizes
`release-notes-<version>.md` carries the `no-changelog` label, since it has no
entry of its own.

After a patch release, its release note document is ported to `master` in a
separate pull request as before. That pull request also deletes the fragments
on `master` that the patch release announced, since those changes have now
been released. Cherry-picking the pull request that folded the fragments on
the release branch deletes them along with the rest of the port.

CI enforces these steps independently of the `no-changelog` label. A pull
request fails if it leaves a fragment behind whose pull request is already
announced in a release note document. A pull request that bumps
`build/version.go` to a release version requires `release-notes-<version>.md`
to exist and all fragments to be folded into it, for release candidates as
well as final releases. A final release additionally requires a non-empty
contributors section. The release build runs the same check against the tagged
commit.

This deliberately leaves editorial consolidation as a single release-time
change. Normal development pull requests therefore never contend on the same
release note document.

[bitcoin-developer-notes]: https://github.com/bitcoin/bitcoin/blob/master/doc/developer-notes.md#release-notes
[bitcoin-fragment]: https://github.com/bitcoin/bitcoin/blob/1566a4767afd3865ee466d1aa2ce8237cb5c27c1/doc/release-notes-36251.md
[bitcoin-release-process]: https://github.com/bitcoin/bitcoin/blob/master/doc/release-process.md#write-the-release-notes
