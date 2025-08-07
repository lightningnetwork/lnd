# Development Guidelines
1. [Code Documentation and Commenting](#code-documentation-and-commenting)
1. [Code Spacing and Formatting](#code-spacing-and-formatting)
1. [Additional Style Constraints](#additional-style-constraints)
1. [Recommended settings for your editor](#recommended-settings-for-your-editor)
1. [Testing](#testing)
1. [Model Git Commit Messages](#model-git-commit-messages)
1. [Ideal Git Commit Structure](#ideal-git-commit-structure)
1. [Sign Your Git Commits](#sign-your-git-commits)
1. [Pointing to Remote Dependent Branches in Go Modules](#pointing-to-remote-dependent-branches-in-go-modules)
1. [Use of Log Levels](#use-of-log-levels)
1. [Use of Golang submodules](#use-of-golang-submodules)

## Why this emphasis on formatting?

Code in general (and Open Source code specifically) is _read_ by developers many
more times during its lifecycle than it is modified. With this fact in mind, the
Golang language was designed for readability (among other goals).
While the enforced formatting of `go fmt` and some best practices already
eliminate many discussions, the resulting code can still look and feel very
differently among different developers.

We aim to enforce a few additional rules to unify the look and feel of all code
in `lnd` to help improve the overall readability.

## Code Documentation and Commenting

- At a minimum every function must be commented with its intended purpose and
  any assumptions that it makes
- Function comments must always begin with the name of the function per
    [Effective Go](https://golang.org/doc/effective_go.html)
- Function comments should be complete sentences since they allow a wide
    variety of automated presentations such as [godoc.org](https://godoc.org)
- The general rule of thumb is to look at it as if you were completely
  unfamiliar with the code and ask yourself, would this give me enough
  information to understand what this function does and how I'd probably want to
  use it?
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

Blocks of code within `lnd` should be segmented into logical stanzas of
operation. Such spacing makes the code easier to follow at a skim, and reduces
unnecessary line noise. Coupled with the commenting scheme specified in the
[contribution guide](#code-documentation-and-commenting),
proper spacing allows readers to quickly scan code, extracting semantics quickly.
Functions should _not_ just be laid out as a bare contiguous block of code.

**WRONG**
```go
	witness := make([][]byte, 4)
	witness[0] = nil
	if bytes.Compare(pubA, pubB) == -1 {
		witness[1] = sigB
		witness[2] = sigA
	} else {
		witness[1] = sigA
		witness[2] = sigB
	}
	witness[3] = witnessScript
	return witness
```
**RIGHT**
```go
	witness := make([][]byte, 4)

	// When spending a p2wsh multi-sig script, rather than an OP_0, we add
	// a nil stack element to eat the extra pop.
	witness[0] = nil

	// When initially generating the witnessScript, we sorted the serialized
	// public keys in descending order. So we do a quick comparison in order
	// to ensure the signatures appear on the Script Virtual Machine stack in
	// the correct order.
	if bytes.Compare(pubA, pubB) == -1 {
		witness[1] = sigB
		witness[2] = sigA
	} else {
		witness[1] = sigA
		witness[2] = sigB
	}

	// Finally, add the preimage as the last witness element.
	witness[3] = witnessScript

	return witness
```

Additionally, we favor spacing between stanzas within syntax like: switch case
statements and select statements.

**WRONG**
```go
	switch {
		case a:
			<code block>
		case b:
			<code block>
		case c:
			<code block>
		case d:
			<code block>
		default:
			<code block>
	}
```
**RIGHT**
```go
	switch {
		// Brief comment detailing instances of this case (repeat below).
		case a:
			<code block>

		case b:
			<code block>

		case c:
			<code block>

		case d:
			<code block>

		default:
			<code block>
	}
```

## Additional Style Constraints

Before a PR is submitted, the proposer should ensure that the file passes the
set of linting scripts run by `make lint`. These include `gofmt`. In addition
to `gofmt` we've opted to enforce the following style guidelines.

### 80 character line length

ALL columns (on a best effort basis) should be wrapped to 80 line columns.
Editors should be set to treat a **tab as 8 spaces**.

**WRONG**
```go
myKey := "0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366a2e151fcee7"
```

**RIGHT**
```go
myKey := "0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366a2e1" +
	"51fcee7"
```

### Wrapping long function calls

When wrapping a line that contains a function call as the unwrapped line exceeds
the column limit, the close parenthesis should be placed on its own line.
Additionally, all arguments should begin in a new line after the open parenthesis.

**WRONG**
```go
value, err := bar(a,
	a, b, c)
```

**RIGHT**
```go
value, err := bar(
	a, a, b, c,
)
```

As long as the visual symmetry of the opening and closing parentheses (or curly
braces) is preserved, arguments that would otherwise introduce a new level of
indentation are allowed to be written in a more compact form.
Visual symmetry here means that when two or more opening parentheses or curly
braces are on the same line, then they must also be closed on the same line.
And the closing line needs to have the same indentation level as the opening
line.

Example with inline struct creation:

**ACCEPTABLE**
```go
	response, err := node.AddInvoice(
		ctx, &lnrpc.Invoice{
			Memo:      "invoice",
			ValueMsat: int64(oneUnitMilliSat - 1),
		},
	)
```

**PREFERRED**
```go
	response, err := node.AddInvoice(ctx, &lnrpc.Invoice{
		Memo:      "invoice",
		ValueMsat: int64(oneUnitMilliSat - 1),
	})
```

**WRONG**
```go
	response, err := node.AddInvoice(ctx, &lnrpc.Invoice{
		Memo:      "invoice",
		ValueMsat: int64(oneUnitMilliSat - 1)})
```

Example with nested function call:

**ACCEPTABLE**:
```go
	payInvoiceWithSatoshi(
		t.t, dave, invoiceResp2, withFailure(
			lnrpc.Payment_FAILED, failureNoRoute,
        ),
    )
```

**PREFERRED**:
```go
	payInvoiceWithSatoshi(t.t, dave, invoiceResp2, withFailure(
		lnrpc.Payment_FAILED, failureNoRoute,
	))
```

#### Exception for log and error message formatting

**Note that the above guidelines don't apply to log or error messages.** For
log and error messages, committers should attempt to minimize the number of
lines utilized, while still adhering to the 80-character column limit. For
example:

**WRONG**
```go
return fmt.Errorf(
	"this is a long error message with a couple (%d) place holders",
	len(things),
)

log.Debugf(
	"Something happened here that we need to log: %v",
	longVariableNameHere,
)
```

**RIGHT**
```go
return fmt.Errorf("this is a long error message with a couple (%d) place "+
	"holders", len(things))

log.Debugf("Something happened here that we need to log: %v",
	longVariableNameHere)
```

This helps to visually distinguish those formatting statements (where nothing
of consequence happens except for formatting an error message or writing
to a log) from actual method or function calls. This compact formatting should
be used for calls to formatting functions like `fmt.Errorf`,
`log.(Trace|Debug|Info|Warn|Error)f` and `fmt.Printf`.
But not for statements that are important for the flow or logic of the code,
like `require.NoErrorf()`.

#### Exceptions and additional styling for structured logging

When making use of structured logging calls (there are any `btclog.Logger` 
methods ending in `S`), a few different rules and exceptions apply.

1) **Static messages:** Structured log calls take a `context.Context` as a first
parameter and a _static_ string as the second parameter (the `msg` parameter). 
Formatted strings should ideally not be used for the construction of the `msg` 
parameter. Instead, key-value pairs (or `slog` attributes) should be used to 
provide additional variables to the log line.

**WRONG**
```go
log.DebugS(ctx, fmt.Sprintf("User %d just spent %.8f to open a channel", userID, 0.0154))
```

**RIGHT**
```go
log.InfoS(ctx, "Channel open performed",
        slog.Int("user_id", userID),
        btclog.Fmt("amount", "%.8f", 0.00154))
```

2) **Key-value attributes**: The third parameter in any structured log method is 
a variadic list of the `any` type but it is required that these are provided in 
key-value pairs such that an associated `slog.Attr` variable can be created for 
each key-value pair. The simplest way to specify this is to directly pass in the 
key-value pairs as raw literals as follows:

```go
log.InfoS(ctx, "Channel open performed", "user_id", userID, "amount", 0.00154)
```
This does work, but it becomes easy to make a mistake and accidentally leave out
a value for each key provided leading to a nonsensical log line. To avoid this, 
it is suggested to make use of the various `slog.Attr` helper functions as 
follows:

```go
log.InfoS(ctx, "Channel open performed",
        slog.Int("user_id", userID),
        btclog.Fmt("amount", "%.8f", 0.00154))
```

3) **Line wrapping**: Structured log lines are an exception to the 80-character
line wrapping rule. This is so that the key-value pairs can be easily read and
reasoned about. If it is the case that there is only a single key-value pair
and the entire log line is still less than 80 characters, it is acceptable to
have the key-value pair on the same line as the log message. However, if there
are multiple key-value pairs, it is suggested to use the one line per key-value
pair format. Due to this suggestion, it is acceptable for any single key-value
pair line to exceed 80 characters for the sake of readability.

**WRONG**
```go
// Example 1.
log.InfoS(ctx, "User connected", 
        "user_id", userID)

// Example 2.
log.InfoS(ctx, "Channel open performed", "user_id", userID,
        btclog.Fmt("amount", "%.8f", 0.00154), "channel_id", channelID)

// Example 3.
log.InfoS(ctx, "Bytes received", 
        "user_id", userID, 
        btclog.Hex("peer_id", peerID.SerializeCompressed()),
        btclog.Hex("message", []bytes{
                0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
        })))
```

**RIGHT**
```go
// Example 1.
log.InfoS(ctx, "User connected", "user_id", userID)

// Example 2.
log.InfoS(ctx, "Channel open performed",
        slog.Int("user_id", userID),
        btclog.Fmt("amount", "%.8f", 0.00154),
        slog.String("channel_id", channelID))

// Example 3.
log.InfoS(ctx, "Bytes received", 
        "user_id", userID,
        btclog.Hex("peer_id", peerID.SerializeCompressed()),
        btclog.Hex("message", []bytes{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})))
```

### Wrapping long function definitions

If one is forced to wrap lines of function arguments that exceed the 
80-character limit, then indentation must be kept on the following lines. Also,
lines should not end with an open parenthesis if the function definition isn't
finished yet.

**WRONG**
```go
func foo(a, b, c,
) (d, error) {

func bar(a, b, c) (
	d, error,
) {

func baz(a, b, c) (
	d, error) {
```
**RIGHT**
```go
func foo(a, b,
	c) (d, error) {

func baz(a, b, c) (d,
	error) {

func longFunctionName(
	a, b, c) (d, error) {
```

If a function declaration spans multiple lines the body should start with an
empty line to help visually distinguishing the two elements.

**WRONG**
```go
func foo(a, b, c,
	d, e) error {
	var a int
}
```
**RIGHT**
```go
func foo(a, b, c,
	d, e) error {

	var a int
}
```

### Inline slice definitions

In Go a list of slices can be initialized with values directly, using curly
braces. Whenever possible, the more verbose/indented style should be used for
better readability and easier git diff handling. Because that results in more
levels of code indentation, the more compact version is allowed in situations
where the remaining space would otherwise be too restricted, resulting in too
long lines (or excessive use of the `// nolint: ll` directive).

**ACCEPTABLE**
```go
	testCases := []testCase{{
		name: "spend exactly all",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * btcutil.SatoshiPerBitcoin,
			},
		}},
	}, {
		name: "spend more",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * btcutil.SatoshiPerBitcoin,
			},
		}},
	}}
```

**PREFERRED**
```go
	coin := btcutil.SatoshiPerBitcoin
	testCases := []testCase{
		{
			name: "spend exactly all",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * coin,
					},
				},
			},
		},
		{
			name: "spend more",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * coin,
					},
				},
			},
		},
	}
```

## Recommended settings for your editor

To make it easier to follow the rules outlined above, we recommend setting up
your editor with at least the following two settings:

1. Set your tabulator width (also called "tab size") to **8 spaces**.
2. Set a ruler or visual guide at 80 character.

Note that the two above settings are automatically applied in editors that
support the `EditorConfig` scheme (for example GoLand, GitHub, GitLab,
VisualStudio). In addition, specific settings for Visual Studio Code are checked
into the code base as well.

Other editors (for example Atom, Notepad++, Vim, Emacs and so on) might install
a plugin to understand the rules in the `.editorconfig` file.

In Vim, you might want to use `set colorcolumn=80`.


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
test coverage tools, see the [golang cover blog post](https://blog.golang.org/cover).

A quick summary of test practices follows:

- All new code should be accompanied by tests that ensure the code behaves
  correctly when given expected values, and, perhaps even more importantly, that
  it handles errors gracefully.
- When you fix a bug, it should be accompanied by tests which exercise the bug
  to both prove it has been resolved and to prevent future regressions.
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


Before committing the changes made, you should run unit tests to validate the
changes, and provide a new integration test when necessary. The unit tests
should pass at two levels,

- Run `make unit-debug log="stdlog trace" pkg=$pkg case=$case timeout=10s`,
  where `pkg` is the package that's updated, and `case` is the newly added or
  affected unit test. This command should run against all the newly added and
  affected test cases. In addition, you should pay attention to the logs added
  here, and make sure they are correctly formatted and no spammy. Also notice
  the timeout - 10 seconds should be more than enough to run a single unit test
  case, if it takes longer than that, consider break the test to make it more
  "unit".

- Run `make unit pkg=$pkg timeout=5m` to make sure all existing unit tests still
  pass.

- If there are newly added integration tests, or the changes may alter the
  workflow of specific areas, run `make itest icase=$icase` to validate the
  behavior, where `icase` is the affected test case.


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
    individual commits that begin to integrate the functionality within the
    codebase.
  * If a PR only fixes a trivial issue, such as updating documentation on a
    small scale, fix typos, or any changes that do not modify the code, the
    commit message of the HEAD commit of the PR should end with `[skip ci]` to
    skip the CI checks. When pushing to such an existing PR, the latest commit
    being pushed should end with `[skip ci]` as to not inadvertently trigger the
    CI checks.
    
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
