# Code formatting rules

## Why this emphasis on formatting?

Code in general (and Open Source code specifically) is _read_ by developers many
more times during its lifecycle than it is modified. With this fact in mind, the
Golang language was designed for readability (among other goals).
While the enforced formatting of `go fmt` and some best practices already
eliminate many discussions, the resulting code can still look and feel very
differently among different developers.

We aim to enforce a few additional rules to unify the look and feel of all code
in `lnd` to help improve the overall readability.

## Spacing

Blocks of code within `lnd` should be segmented into logical stanzas of
operation. Such spacing makes the code easier to follow at a skim, and reduces
unnecessary line noise. Coupled with the commenting scheme specified above,
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
the column limit, the close paren should be placed on its own line.
Additionally, all arguments should begin in a new line after the open paren.

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

### Wrapping long function definitions

If one is forced to wrap lines of function arguments that exceed the 80
character limit, then indentation must be kept on the following lines. Also,
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

## Recommended settings for your editor

To make it easier to follow the rules outlined above, we recommend setting up
your editor with at least the following two settings:

1. Set your tabulator width (also called "tab size") to **8 spaces**.
2. Set a ruler or visual guide at 80 character.

Note that the two above settings are automatically applied in editors that
support the `EditorConfig` scheme (for example GoLand, GitHub, GitLab,
VisualStudio). In addition, specific settings for Visual Studio Code are checked
into the code base as well.

Other editors (for example Atom, Nodepad++, Vim, Emacs and so on) might install
a plugin to understand the rules in the `.editorconfig` file.

In Vim you might want to use `set colorcolumn=80`.
