# Table of Contents
1. [Overview](#overview)
2. [Minimum Recommended Skillset](#minimum-recommended-skillset)
3. [Required Reading](#required-reading)
4. [Substantial contributions only](#substantial-contributions-only)
5. [Development Practices](#development-practices)
   1. [Share Early, Share Often](#share-early-share-often)
   1. [Development Guidelines](#development-guidelines)
5. [Code Approval Process](#code-approval-process)
   1. [Code Review](#code-review)
   1. [Rework Code (if needed)](#rework-code-if-needed)
   1. [Acceptance](#acceptance)
   1. [Backporting Changes](#backporting-changes)
   1. [Review Bot](#review-bot)
7. [Contribution Standards](#contribution-standards)
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
of low-hanging fruit which can be tackled without having full competency in the
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

- [Effective Go](https://golang.org/doc/effective_go.html) - The entire `lnd` 
  project follows the guidelines in this document.  For your code to be accepted,
  it must follow the guidelines therein.
- [Original Satoshi Whitepaper](https://bitcoin.org/bitcoin.pdf) - This is the white paper that started it all.  Having a solid
  foundation to build on will make the code much more comprehensible.
- [Lightning Network Whitepaper](https://lightning.network/lightning-network-paper.pdf) - This is the white paper that kicked off the Layer 2 revolution. Having a good grasp of the concepts of Lightning will make the core logic within the daemon much more comprehensible: Bitcoin Script, off-chain blockchain protocols, payment channels, bidirectional payment channels, relative and absolute time-locks, commitment state revocations, and Segregated Witness. 
    - The original LN was written for a rather narrow audience, the paper may be a bit unapproachable to many. Thanks to the Bitcoin community, there exist many easily accessible supplemental resources which can help one see how all the pieces fit together from double-spend protection all the way up to commitment state transitions and Hash Time Locked Contracts (HTLCs): 
        - [Lightning Network Summary](https://lightning.network/lightning-network-summary.pdf)
        - [Understanding the Lightning Network 3-Part series](https://bitcoinmagazine.com/technical/understanding-the-lightning-network-part-building-a-bidirectional-payment-channel-1464710791) 
        - [Deployable Lightning](https://github.com/ElementsProject/lightning/blob/master/doc/miscellaneous/deployable-lightning.pdf)


Note that the core design of the Lightning Network has shifted over time as
concrete implementation and design has expanded our knowledge beyond the
original white paper. Therefore, specific information outlined in the resources
above may be a bit out of date. Many implementers are currently working on an
initial [Lightning Network Specifications](https://github.com/lightningnetwork/lightning-rfc).
Once the specification is finalized, it will be the most up-to-date
comprehensive document explaining the Lightning Network. As a result, it will
be recommended for newcomers to read first in order to get up to speed. 

# Substantial contributions only

Due to the prevalence of automated analysis and pull request authoring tools
and online competitions that incentivize creating commits in popular
repositories, the maintainers of this project are flooded with trivial pull
requests that only change some typos or other insubstantial content (e.g. the
year in the license file).
If you are an honest user that wants to contribute to this project, please
consider that every pull request takes precious time from the maintainers to
review and consider the impact of changes. Time that could be spent writing
features or fixing bugs.
If you really want to contribute, [consider reviewing and testing other users'
pull requests instead](review.md).
First-time reviewer friendly [pull requests can be found
here](https://github.com/lightningnetwork/lnd/pulls?q=is%3Aopen+is%3Apr+label%3A%22good+first+review%22).
Once you are familiar with the project's code style, testing and review
procedure, your own pull requests will likely require less guidance and fewer
maintainer review cycles, resulting in potentially faster merges.
Also, consider increasing the test coverage of the code by writing more unit
tests first, which is also a very valuable way to contribute and learn more
about the code base.

# Development Practices

Developers are expected to work in their own trees and submit pull requests when
they feel their feature or bug fix is ready for integration into the master
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

## Development Guidelines

The `lnd` project emphasizes code readability and maintainability through
specific development guidelines. Key aspects include: thorough code
documentation with clear function comments and meaningful inline comments;
consistent code spacing to separate logical blocks; adherence to an 80-character
line limit with specific rules for wrapping function calls, definitions, and log
messages (including structured logging); comprehensive unit and integration
testing for all changes; well-structured Git commit messages with package
prefixes and atomic commits; signing Git commits; proper handling of Go module
dependencies and submodules; and appropriate use of log levels. Developers are
encouraged to configure their editors to align with these standards.

For the complete set of rules and examples, please refer to the detailed
[Development Guidelines](development_guidelines.md).


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
days, while large or far-reaching changes may take weeks.  This is a good reason
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
The reason for this is twofold: it makes it easier for the reviewer to see
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

## Backporting Changes

After a PR is merged to master, it may need to be backported to release branches
(e.g., `v0.20.x-branch`) to include the fix or feature in upcoming patch releases.

The project uses an **automated backport workflow** to simplify this process. Simply
add a label like `backport-v0.20.x-branch` to your merged PR, and a GitHub Action
will automatically create a backport PR for you.

For complete documentation on the automated backport workflow, including:
- How to use backport labels
- Handling merge conflicts
- Multiple backports
- Troubleshooting

See [backport-workflow.md](backport-workflow.md)

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
used. This also means you don't need to keep adding new control comments, just 
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
