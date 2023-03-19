# Table of Contents
1. [Overview](#overview)
2. [Minimum Recommended Skillset](#minimum-recommended-skillset)
3. [Required Reading](#required-reading)
4. [Development Practices](#development-practices)
   1. [Share Early, Share Often](#share-early-share-often)
   1. [Testing](#testing)
   1. [Code Documentation and Commenting](#code-documentation-and-commenting)
   1. [Model Git Commit Messages](#model-git-commit-messages)
   1. [Ideal Git Commit Structure](#ideal-git-commit-structure)
   1. [Sign Your Git Commits](#sign-your-git-commits)
   1. [Code Spacing and formatting](#code-spacing-and-formatting)
   1. [Pointing to Remote Dependent Branches in Go Modules](#pointing-to-remote-dependent-branches-in-go-modules)
   1. [Use of Log Levels](#use-of-log-levels)
   1. [Use of Golang submodules](#use-of-golang-submodules)
5. [Code Approval Process](#code-approval-process)
   1. [Code Review](#code-review)
   1. [Rework Code (if needed)](#rework-code-if-needed)
   1. [Acceptance](#acceptance)
   1. [Review Bot](#review-bot)
6. [Contribution Standards](#contribution-standards)
   1. [Contribution Checklist](#contribution-checklist)
   1. [Licensing of Contributions](#licensing-of-contributions)

# Overview

Developing cryptocurrencies is an exciting endeavor that touches a wide variety
of areas such as wire protocols, peer-to-peer networking, databases,
cryptography, language interpretation (transaction scripts), adversarial
threat-modeling, and RPC systems. They also represent a radical shift to the
current monetary system and as a result provide an opportunity to help reshape
the entire financial system. With the advent of the [Lightning Network
(LN)](https://lightning.network/), new layers are being constructed upon the
base blockchain layer which have the potential to alleviate many of the
limitations and constraints inherent in the design of blockchains. There are
few projects that offer this level of diversity and impact all in one code
base. 

However, as exciting as it is, one must keep in mind that cryptocurrencies
represent real money and introducing bugs and security vulnerabilities can have
far more dire consequences than in typical projects where having a small bug is
minimal by comparison.  In the world of cryptocurrencies, even the smallest bug
in the wrong area can cost people a significant amount of money.  For this
reason, the Lightning Network Daemon (`lnd`) has a formalized and rigorous
development process (heavily inspired by
[btcsuite](https://github.com/btcsuite)) which is outlined on this page.

We highly encourage code contributions, however it is imperative that you adhere
to the guidelines established on this page.

# Minimum Recommended Skillset

The following list is a set of core competencies that we recommend you possess
before you really start attempting to contribute code to the project.  These are
not hard requirements as we will gladly accept code contributions as long as
they follow the guidelines set forth on this page.  That said, if you don't have
the following basic qualifications you will likely find it quite difficult to
contribute to the core layers of Lightning. However, there are still a number
of low hanging fruit which can be tackled without having full competency in the
areas mentioned below. 

- A reasonable understanding of bitcoin at a high level (see the
  [Required Reading](#required-reading) section for the original white paper)
- A reasonable understanding of the Lightning Network at a high level
- Experience in some type of C-like language
- An understanding of data structures and their performance implications
- Familiarity with unit testing
- Debugging experience
- Ability to understand not only the area you are making a change in, but also
  the code your change relies on, and the code which relies on your changed code

Building on top of those core competencies, the recommended skill set largely
depends on the specific areas you are looking to contribute to.  For example,
if you wish to contribute to the cryptography code, you should have a good
understanding of the various aspects involved with cryptography such as the
security and performance implications.

# Required Reading

- [Effective Go](http://golang.org/doc/effective_go.html) - The entire `lnd` 
  project follows the guidelines in this document.  For your code to be accepted,
  it must follow the guidelines therein.
- [Original Satoshi Whitepaper](https://bitcoin.org/bitcoin.pdf) - This is the white paper that started it all.  Having a solid
  foundation to build on will make the code much more comprehensible.
- [Lightning Network Whitepaper](https://lightning.network/lightning-network-paper.pdf) - This is the white paper that kicked off the Layer 2 revolution. Having a good grasp of the concepts of Lightning will make the core logic within the daemon much more comprehensible: Bitcoin Script, off-chain blockchain protocols, payment channels, bi-directional payment channels, relative and absolute time-locks, commitment state revocations, and Segregated Witness. 
    - The original LN was written for a rather narrow audience, the paper may be a bit unapproachable to many. Thanks to the Bitcoin community, there exist many easily accessible supplemental resources which can help one see how all the pieces fit together from double-spend protection all the way up to commitment state transitions and Hash Time Locked Contracts (HTLCs): 
        - [Lightning Network Summary](https://lightning.network/lightning-network-summary.pdf)
        - [Understanding the Lightning Network 3-Part series](https://bitcoinmagazine.com/articles/understanding-the-lightning-network-part-building-a-bidirectional-payment-channel-1464710791) 
        - [Deployable Lightning](https://github.com/ElementsProject/lightning/blob/master/doc/deployable-lightning.pdf) 


Note that the core design of the Lightning Network has shifted over time as
concrete implementation and design has expanded our knowledge beyond the
original white paper. Therefore, specific information outlined in the resources
above may be a bit out of date. Many implementers are currently working on an
initial [Lightning Network Specifications](https://github.com/lightningnetwork/lightning-rfc).
Once the specification is finalized, it will be the most up-to-date
comprehensive document explaining the Lightning Network. As a result, it will
be recommended for newcomers to read first in order to get up to speed. 

# Development Practices

Developers are expected to work in their own trees and submit pull requests when
they feel their feature or bug fix is ready for integration into the  master
branch.

## Share Early, Share Often

We firmly believe in the share early, share often approach.  The basic premise
of the approach is to announce your plans **before** you start work, and once
you have started working, craft your changes into a stream of small and easily
reviewable commits.

This approach has several benefits:

- Announcing your plans to work on a feature **before** you begin work avoids
  duplicate work
- It permits discussions which can help you achieve your goals in a way that is
  consistent with the existing architecture
- It minimizes the chances of you spending time and energy on a change that
  might not fit with the consensus of the community or existing architecture and
  potentially be rejected as a result
- The quicker your changes are merged to master, the less time you will need to
  spend rebasing and otherwise trying to keep up with the main code base

## Testing

One of the major design goals of all of `lnd`'s packages and the daemon itself is
to aim for a high degree of test coverage.  This is financial software so bugs
and regressions in the core logic can cost people real money.  For this reason
every effort must be taken to ensure the code is as accurate and bug-free as
possible.  Thorough testing is a good way to help achieve that goal.

Unless a new feature you submit is completely trivial, it will probably be
rejected unless it is also accompanied by adequate test coverage for both
positive and negative conditions.  That is to say, the tests must ensure your
code works correctly when it is fed correct data as well as incorrect data
(error paths).


Go provides an excellent test framework that makes writing test code and
checking coverage statistics straightforward.  For more information about the
test coverage tools, see the [golang cover blog post](http://blog.golang.org/cover).

A quick summary of test practices follows:
- All new code should be accompanied by tests that ensure the code behaves
  correctly when given expected values, and, perhaps even more importantly, that
  it handles errors gracefully
- When you fix a bug, it should be accompanied by tests which exercise the bug
  to both prove it has been resolved and to prevent future regressions
- Changes to publicly exported packages such as
  [brontide](https://github.com/lightningnetwork/lnd/tree/master/brontide) should
  be accompanied by unit tests exercising the new or changed behavior.
- Changes to behavior within the daemon's interaction with the P2P protocol,
  or RPC's will need to be accompanied by integration tests which use the
  [`networkHarness`framework](https://github.com/lightningnetwork/lnd/blob/master/lntest/harness.go)
  contained within `lnd`. For example integration tests, see
  [`lnd_test.go`](https://github.com/lightningnetwork/lnd/blob/master/itest/lnd_test.go).
- The itest log files are automatically scanned for `[ERR]` lines. There
  shouldn't be any of those in the logs, see [Use of Log Levels](#use-of-log-levels).

Throughout the process of contributing to `lnd`, you'll likely also be
extensively using the commands within our `Makefile`. As a result, we recommend
[perusing the make file documentation](https://github.com/lightningnetwork/lnd/blob/master/docs/MAKEFILE.md).

## Code Documentation and Commenting

- At a minimum every function must be commented with its intended purpose and
  any assumptions that it makes
  - Function comments must always begin with the name of the function per
    [Effective Go](http://golang.org/doc/effective_go.html)
  - Function comments should be complete sentences since they allow a wide
    variety of automated presentations such as [godoc.org](https://godoc.org)
  - The general rule of thumb is to look at it as if you were completely
    unfamiliar with the code and ask yourself, would this give me enough
	information to understand what this function does and how I'd probably want
	to use it?
- Exported functions should also include detailed information the caller of the
  function will likely need to know and/or understand:<br /><br />

**WRONG**
```go
// generates a revocation key
func DeriveRevocationPubkey(commitPubKey *btcec.PublicKey,
	revokePreimage []byte) *btcec.PublicKey {
```
**RIGHT**
```go
// DeriveRevocationPubkey derives the revocation public key given the
// counterparty's commitment key, and revocation preimage derived via a
// pseudo-random-function. In the event that we (for some reason) broadcast a
// revoked commitment transaction, then if the other party knows the revocation
// preimage, then they'll be able to derive the corresponding private key to
// this private key by exploiting the homomorphism in the elliptic curve group:
//    * https://en.wikipedia.org/wiki/Group_homomorphism#Homomorphisms_of_abelian_groups
//
// The derivation is performed as follows:
//
//   revokeKey := commitKey + revokePoint
//             := G*k + G*h
//             := G * (k+h)
//
// Therefore, once we divulge the revocation preimage, the remote peer is able to
// compute the proper private key for the revokeKey by computing:
//   revokePriv := commitPriv + revokePreimge mod N
//
// Where N is the order of the sub-group.
func DeriveRevocationPubkey(commitPubKey *btcec.PublicKey,
	revokePreimage []byte) *btcec.PublicKey {
```
- Comments in the body of the code are highly encouraged, but they should
  explain the intention of the code as opposed to just calling out the
  obvious<br /><br />

**WRONG**
```go
// return err if amt is less than 546
if amt < 546 {
	return err
}
```
**RIGHT**
```go
// Treat transactions with amounts less than the amount which is considered dust
// as non-standard.
if amt < 546 {
	return err
}
```
**NOTE:** The above should really use a constant as opposed to a magic number,
but it was left as a magic number to show how much of a difference a good
comment can make.

## Code Spacing and formatting

Code in general (and Open Source code specifically) is _read_ by developers many
more times during its lifecycle than it is modified. With this fact in mind, the
Golang language was designed for readability (among other goals).
While the enforced formatting of `go fmt` and some best practices already
eliminate many discussions, the resulting code can still look and feel very
differently among different developers.

We aim to enforce a few additional rules to unify the look and feel of all code
in `lnd` to help improve the overall readability.

**Please refer to the [code formatting rules
document](./code_formatting_rules.md)** to see the list of additional style
rules we enforce.

## Model Git Commit Messages

This project prefers to keep a clean commit history with well-formed commit
messages.  This section illustrates a model commit message and provides a bit
of background for it.  This content was originally created by Tim Pope and made
available on his website, however that website is no longer active, so it is
being provided here.

Here’s a model Git commit message:

```text
Short (50 chars or less) summary of changes

More detailed explanatory text, if necessary.  Wrap it to about 72
characters or so.  In some contexts, the first line is treated as the
subject of an email and the rest of the text as the body.  The blank
line separating the summary from the body is critical (unless you omit
the body entirely); tools like rebase can get confused if you run the
two together.

Write your commit message in the present tense: "Fix bug" and not "Fixed
bug."  This convention matches up with commit messages generated by
commands like git merge and git revert.

Further paragraphs come after blank lines.

- Bullet points are okay, too
- Typically a hyphen or asterisk is used for the bullet, preceded by a
  single space, with blank lines in between, but conventions vary here
- Use a hanging indent
```

Here are some of the reasons why wrapping your commit messages to 72 columns is
a good thing.

- git log doesn't do any special wrapping of the commit messages. With
  the default pager of less -S, this means your paragraphs flow far off the edge
  of the screen, making them difficult to read. On an 80 column terminal, if we
  subtract 4 columns for the indent on the left and 4 more for symmetry on the
  right, we’re left with 72 columns.
- git format-patch --stdout converts a series of commits to a series of emails,
  using the messages for the message body.  Good email netiquette dictates we
  wrap our plain text emails such that there’s room for a few levels of nested
  reply indicators without overflow in an 80 column terminal.
  
In addition to the Git commit message structure adhered to within the daemon
all short-[commit messages are to be prefixed according to the convention
outlined in the Go project](https://golang.org/doc/contribute.html#change). All
commits should begin with the subsystem or package primarily affected by the
change. In the case of a widespread change, the packages are to be delimited by
either a '+' or a ','. This prefix seems minor but can be extremely helpful in
determining the scope of a commit at a glance, or when bug hunting to find a
commit which introduced a bug or regression. 

## Ideal Git Commit Structure

Within the project we prefer small, contained commits for a pull request over a
single giant commit that touches several files/packages. Ideal commits build on
their own, in order to facilitate easy usage of tools like `git bisect` to `git
cherry-pick`. It's preferred that commits contain an isolated change in a
single package. In this case, the commit header message should begin with the
prefix of the modified package. For example, if a commit was made to modify the
`lnwallet` package, it should start with `lnwallet: `. 

In the case of changes that only build in tandem with changes made in other
packages, it is permitted for a single commit to be made which contains several
prefixes such as: `lnwallet+htlcswitch`. This prefix structure along with the
requirement for atomic contained commits (when possible) make things like
scanning the set of commits and debugging easier. In the case of changes that
touch several packages, and can only compile with the change across several
packages, a `multi: ` prefix should be used.

Examples of common patterns w.r.t commit structures within the project:

  * It is common that during the work on a PR, existing bugs are found and
    fixed. If they can be fixed in isolation, they should have their own
    commit. 
  * File restructuring like moving a function to another file or changing order
    of functions: with a separate commit because it is much easier to review
    the real changes that go on top of the restructuring.
  * Preparatory refactorings that are functionally equivalent: own commit.
  * Project or package wide file renamings should be in their own commit.
  * Ideally if a new package/struct/sub-system is added in a PR, there should
    be a single commit which adds the new functionality, with follow up
    induvidual commits that begin to intergrate the functionality within the
    codebase.
  * If a PR only fixes a trivial issue, such as updating documentation on a
    small scale, fix typos, or any changes that do not modify the code, the
    commit message should end with `[skip ci]` to skip the CI checks.
    
## Sign your git commits

When contributing to `lnd` it is recommended to sign your git commits. This is
easy to do and will help in assuring the integrity of the tree. See [mailing
list entry](https://lists.linuxfoundation.org/pipermail/bitcoin-dev/2014-May/005877.html)
for more information.

### How to sign your commits?

Provide the `-S` flag (or `--gpg-sign`) to git commit when you commit
your changes, for example

```shell
$  git commit -m "Commit message" -S
```

Optionally you can provide a key id after the `-S` option to sign with a
specific key.

To instruct `git` to auto-sign every commit, add the following lines to your
`~/.gitconfig` file:

```text
[commit]
        gpgsign = true
```

### What if I forgot?

You can retroactively sign your previous commit using `--amend`, for example

```shell
$  git commit -S --amend
```

If you need to go further back, you can use the interactive rebase
command with 'edit'. Replace `HEAD~3` with the base commit from which
you want to start.

```shell
$  git rebase -i HEAD~3
```

Replace 'pick' by 'edit' for the commit that you want to sign and the
rebasing will stop after that commit. Then you can amend the commit as
above. Afterwards, do

```shell
$  git rebase --continue
```

As this will rewrite history, you cannot do this when your commit is
already merged. In that case, too bad, better luck next time.

If you rewrite history for another reason - for example when squashing
commits - make sure that you re-sign as the signatures will be lost.

Multiple commits can also be re-signed with `git rebase`. For example, signing
the last three commits can be done with:

```shell
$  git rebase --exec 'git commit --amend --no-edit -n -S' -i HEAD~3
```

### How to check if commits are signed?

Use `git log` with `--show-signature`,

```shell
$  git log --show-signature
```

You can also pass the `--show-signature` option to `git show` to check a single
commit.

## Pointing to Remote Dependent Branches in Go Modules

It's common that a developer may need to make a change in a dependent project
of `lnd` such as `btcd`, `neutrino`, `btcwallet`, etc. In order to test changes
without testing infrastructure, or simply make a PR into `lnd` that will build
without any further work, the `go.mod` and `go.sum` files will need to be
updated. Luckily, the `go mod` command has a handy tool to do this
automatically so developers don't need to manually edit the `go.mod` file:
```shell
$  go mod edit -replace=IMPORT-PATH-IN-LND@LND-VERSION=DEV-FORK-IMPORT-PATH@DEV-FORK-VERSION
```

Here's an example replacing the `lightning-onion` version checked into `lnd` with a version in roasbeef's fork:
```shell
$  go mod edit -replace=github.com/lightningnetwork/lightning-onion@v0.0.0-20180605012408-ac4d9da8f1d6=github.com/roasbeef/lightning-onion@2e5ae87696046298365ab43bcd1cf3a7a1d69695
```

## Use of Log Levels

There are six log levels available: `trace`, `debug`, `info`, `warn`, `error` and `critical`.

Only use `error` for internal errors that are never expected to happen during
normal operation. No event triggered by external sources (rpc, chain backend,
etc) should lead to an `error` log.

## Use of Golang submodules

Changes to packages that are their own submodules (e.g. they contain a `go.mod`
and `go.sum` file, for example `tor/go.mod`) require a specific process.
We want to avoid the use of local replace directives in the root `go.mod`,
therefore changes to a submodule are a bit involved.

The main process for updating and then using code in a submodule is as follows:
 - Create a PR for the changes to the submodule itself (e.g. edit something in
   the `tor` package)
 - Wait for the PR to be merged and a new tag (for example `tor/v1.0.x`) to be
   pushed.
 - Create a second PR that bumps the updated submodule in the root `go.mod` and
   uses the new functionality in the main module.

Of course the two PRs can be opened at the same time and be built on top of each
other. But the merge and tag push order should always be maintained.

# Code Approval Process

This section describes the code approval process that is used for code
contributions.  This is how to get your changes into `lnd`.

## Code Review

All code which is submitted will need to be reviewed before inclusion into the
master branch.  This process is performed by the project maintainers and usually
other committers who are interested in the area you are working in as well.

### Code Review Timeframe

The timeframe for a code review will vary greatly depending on factors such as
the number of other pull requests which need to be reviewed, the size and
complexity of the contribution, how well you followed the guidelines presented
on this page, and how easy it is for the reviewers to digest your commits.  For
example, if you make one monolithic commit that makes sweeping changes to things
in multiple subsystems, it will obviously take much longer to review.  You will
also likely be asked to split the commit into several smaller, and hence more
manageable, commits.

Keeping the above in mind, most small changes will be reviewed within a few
days, while large or far reaching changes may take weeks.  This is a good reason
to stick with the [Share Early, Share Often](#share-early-share-often)
development practice outlined above.

### What is the review looking for?

The review is mainly ensuring the code follows the
[Development Practices](#development-practices) and
[Code Contribution Standards](#contribution-standards). However, there are a few
other checks which are generally performed as follows:

- The code is stable and has no stability or security concerns
- The code is properly using existing APIs and generally fits well into the
  overall architecture
- The change is not something which is deemed inappropriate by community
  consensus

## Rework Code (if needed)

After the code review, the change will be accepted immediately if no issues are
found.  If there are any concerns or questions, you will be provided with
feedback along with the next steps needed to get your contribution merged with
master.  In certain cases the code reviewer(s) or interested committers may help
you rework the code, but generally you will simply be given feedback for you to
make the necessary changes.

During the process of responding to review comments, we prefer that changes be
made with [fixup commits](https://robots.thoughtbot.com/autosquashing-git-commits). 
The reason for this is two fold: it makes it easier for the reviewer to see
what changes have been made between versions (since Github doesn't easily show
prior versions like Critique) and it makes it easier on the PR author as they
can set it to auto squash the fix up commits on rebase.

This process will continue until the code is finally accepted.

## Acceptance

Before your code is accepted, the [release notes we keep in-tree for the next
upcoming milestone should be extended to describe the changes contained in your
PR](https://github.com/lightningnetwork/lnd/tree/master/docs/release-notes).
Unless otherwise mentioned by the reviewers of your PR, the description of your
changes should live in the document set for the _next_ major release. 

Once your code is accepted, it will be integrated with the master branch. After
2+ (sometimes 1) LGTM's (approvals) are given on a PR, it's eligible to land in
master. At this final phase, it may be necessary to rebase the PR in order to
resolve any conflicts and also squash fix up commits. Ideally, the set of
[commits by new contributors are PGP signed](https://git-scm.com/book/en/v2/Git-Tools-Signing-Your-Work), 
although this isn't a strong requirement (but we prefer it!). In order to keep
these signatures intact, we prefer using merge commits. PR proposers can use
`git rebase --signoff` to sign and rebase at the same time as a final step.

Rejoice as you will now be listed as a [contributor](https://github.com/lightningnetwork/lnd/graphs/contributors)!

## Review Bot

In order to keep the review flow going, Lightning Labs uses a bot to remind 
PR reviewers about their outstanding reviews or to remind authors to address 
recent reviews. Here are some important things to know about the bot and some 
controls for adjusting its behaviour:

####🤖 Expected Behaviour:
- The bot will not do anything if your PR is in draft mode.
- It will ping a pending reviewer if they have not reviewed or commented on the 
PR in x days since the last update or the last time the bot pinged them. 
(default x = 3)
- It will ping the author of the PR if they have not addressed a review on a PR 
after x days since last review or the last time the bot pinged them. It will 
also ping them to remind them to re-request review if needed. (default x = 3)

####🤖 Controls:
To control the bot, you need to add a comment on the PR starting with 
`!lightninglabs-deploy` followed by the command. There are 2 control types: 
mute/unmute & cadence. Only the latest comment for each control type will be 
used. This also means you dont need to keep adding new control comments, just 
edit the latest comment for that control type.

- `!lightninglabs-deploy mute` will mute the bot on the PR completely.
- `!lightninglabs-deploy mute 72h30m` will mute the bot for the given duration.
- `!lightninglabs-deploy mute 2022-Feb-02` will mute the bot until the given 
date (must be in this format!).
- `!lightninglabs-deploy mute #4` will mute the bot until the given PR of the 
same repo has been merged.
- `!lightninglabs-deploy unmute` will unmute the bot (or just delete the comment
that was muting it)
- `!lightninglabs-deploy cadence 60h` change the cadence of the bot from the 
default of 3 days to the given duration. 
- it will auto-mute if the PR is in Draft mode

# Contribution Standards

## Contribution Checklist

See [template](https://github.com/lightningnetwork/lnd/blob/master/.github/pull_request_template.md).

## Licensing of Contributions
****
All contributions must be licensed with the
[MIT license](https://github.com/lightningnetwork/lnd/blob/master/LICENSE).  This is
the same license as all of the code found within lnd.


# Acknowledgements
This document was heavily inspired by a [similar document outlining the code
contribution](https://github.com/btcsuite/btcd/blob/master/docs/code_contribution_guidelines.md)
guidelines for btcd. 
