# Code review

This document aims to explain why code review can be a very valuable form of
contribution to any Open Source project.
But code review is a very broad term that can mean many things. Good in-depth
code review is also a skill that needs to be learned but can be highly valuable
in any development team.

## Why should new contributors review code first?

Developers interested in contributing to an Open Source project seem to often
think that _writing code_ is the bottleneck in those projects.
That rarely is the case though. Especially in Bitcoin related projects, where
any mistake can have enormous consequences, any code that is to be merged
needs to go through a thorough review first (often requiring two or more
developers to approve before it can be merged).

Therefore, the actual bottleneck in Bitcoin related Open Source projects is
**review, not the production of code**.

So new contributors that come into a project by opening a PR often take more
time away from the maintainers of that project than what their first
contribution ends up adding in value. That is especially the case if the
contributors don't have a lot of experience with git, GitHub, the target
programming language and its formatting requirements or the review and test
process as a whole.

If a new contributor instead starts reviewing Pull Requests first, before
opening their own ones, they can gain the following advantages:
 - They learn about the project, its formal requirements and the programming
   language used, without anyone's assistance.
 - They learn about how to compile, execute and test the project they are aiming
   to contribute to.
 - They see best practices in terms of formatting, naming, commit structure,
   etc.
 - The value added to the project by reviewing, running and testing code is
   likely positive from day one and requires much less active assistance and
   time from maintainers.

The maintainers of this project therefore ask all new contributors to review
at least a couple of Pull Requests before adding their own.

## How do I review code?

Reviewing code in a constructive and helpful way is almost an art form by
itself. The following list is definitely not complete and every developer tends
to develop their own style when it comes to code review. But the list should
have the most basic tasks associated with code review that should be a good
entry point for anyone not yet familiar with the process:

- Make sure to pick a Pull Request that fits your level of experience. You
  might want to pick one that [has the "good first review"
  label](https://github.com/lightningnetwork/lnd/labels/good%20first%20review).
  If you're new to the area that you'd like to review, first familiarize
  yourself with the code, try to understand the architecture, public interfaces,
  interactions and code coverage. It can be useful to look at previous PRs to
  get more insight.
- Check out the code of the pull request in your local git (check [this
  guide](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/reviewing-changes-in-pull-requests/checking-out-pull-requests-locally)
  for an easy shortcut).
- Compile the code. See [INSTALL.md](INSTALL.md) and [MAKEFILE.md](MAKEFILE.md)
  for inputs on how to do that.
- Run the unit tests of the changed areas. See the [`unit` section of
  MAKEFILE.md](MAKEFILE.md#unit) on how to run specific packages or test cases
  directly.
- Run the code on your local machine and attempt to test the changes of the
  pull request. For any actual Bitcoin or Lightning related functionality, you
  might want to spin up a `regtest` local network to test against. You can check
  out the [`regtest` setup project](https://github.com/guggero/rt) that might
  be helpful for that.
- Try to manually test the changes in the PR. What to actually test might differ
  from PR to PR. For a bug fix you might first want to attempt to reproduce the
  bug in a version prior to the fix. Then test that the Pull Request actually
  fixes the bug.
  Code that introduces new features should be tested by trying to run all the
  new code paths and try out all new command line flags as well as RPC
  methods and fields. Making sure that invalid values and combinations lead to
  proper error messages the user can easily understand is important.
- Keep the logs of your tests as part of your review feedback. If you find a
  bug, the exact steps to reproduce as well as the local logs will be super
  helpful to the author of the Pull Request.
- Propose areas where the unit test coverage could be improved (try to propose
  specific test cases, for example "we should test the behavior with a negative
  number here", instead of just saying "this area needs more tests").
- Bonus: Write a unit test (or test case) that you can propose to add to the PR.
- Go through the PR commit by commit and line by line
    - Is the flow of the commits easy to understand? Does the order make sense?
      See the [code contribution guidelines](development_guidelines.md#ideal-git-commit-structure)
      for more information on this.
    - Is the granularity of the commit structure easy to understand? Should
      commits be split up or be combined?
    - What does each line achieve in terms of the overall goal of the PR?
    - What does each line change in the behavior of the code?
    - Are there any potentially unwanted side effects?
    - Are all errors handled appropriately?
    - Is there sufficient logging to trace what happens if a user runs into
      problems with the code? (e.g. when testing the code, can you find out what
      code path your execution took just by looking at the trace-level log?)
    - Does the commit message appropriately reflect the changes in that commit?
      Only relevant for non-self-explanatory commits.
- Is the documentation added by the PR sufficient?
- Bonus: Propose additional documentation as part of the review that can be
  added to the PR.
- What does the user interface (command line flags, gRPC/REST API) affected by
  the PR look like? Is it easy to understand for users? Are there any unintended
  breaking changes for current users of the API being introduced? We attempt to
  not break existing API users by renaming or changing existing fields.
- Does the CI pass for the PR? If not, can you find out what needs to be changed
  to make the CI pass? Unfortunately there are still some flakes in the tests,
  so some steps might fail unrelated to the changes in the Pull Request.

## How to submit review feedback

The above checklist is very detailed and extensive. But a review can also be
helpful if it only covers some of the steps outlined. The more, the better, of
course, but any review is welcome (within reason of course, don't spam pull
requests with trivial things like typos/naming or obvious AI-generated review
that doesn't add any value).

Just make sure to mention what parts you were focusing on.
The following abbreviations/acronyms might be used to indicate partial or
specific review:

- Examples:
  - cACK (concept acknowledgement): I only looked at the high-level changes, the
    approach looks okay to me (but didn't deeply look at the code or tests).
  - tACK (tested acknowledgement): I didn't look at the code, but I compiled,
    ran and tested the code changes.
  - ACK ("full" acknowledgement): I reviewed all relevant aspects of the PR and
    I have no objections to merging the code.
  - Request changes: I think we should change the following aspects of the PR
    before merging: ...
  - NACK (negative acknowledgement): I think we should _not_ proceed with the
    changes in this PR, because of the following reasons: ... 

Then try to give as much detail as you can. And as with every communication in
Open Source projects: Always be polite and objective in the feedback.
Ask yourself: Would you find the feedback valuable and appropriate if someone
else posted it on your PR?
