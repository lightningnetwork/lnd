# LND Style Guide

## Code Documentation and Commenting

- Always use the Golang code style described below in this document. 
- Readable code is the most important requirement for any commit created. 
- Comments must not explain the code 1:1 but instead explain the _why_ behind a 
  certain block of code, in case it requires contextual knowledge. 
- Unit tests must always use the `require` library. Either table driven unit 
  tests or tests using the `rapid` library are preferred. 
- The line length MUST NOT exceed 80 characters, this is very important. 
  You must count the Golang indentation (tabulator character) as 8 spaces when 
  determining the line length. Use creative approaches or the wrapping rules 
  specified below to make sure the line length isn't exceeded. HOWEVER: during
  code reviews, please leave this check up to the linter and do not comment on
  it (why? because gemini is bad at counting characters).
- Every function must be commented with its purpose and assumptions.
- Function comments must begin with the function name.
- Function comments should be complete sentences.
- Exported functions require detailed comments for the caller.

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
// this private key by exploiting the homomorphism in the elliptic curve group.
//
// The derivation is performed as follows:
//
//   revokeKey := commitKey + revokePoint
//             := G*k + G*h
//             := G * (k+h)
//
// Therefore, once we divulge the revocation preimage, the remote peer is able
// to compute the proper private key for the revokeKey by computing:
//   revokePriv := commitPriv + revokePreimge mod N
//
// Where N is the order of the sub-group.
func DeriveRevocationPubkey(commitPubKey *btcec.PublicKey,
	revokePreimage []byte) *btcec.PublicKey {
```
- In-body comments should explain the *intention* of the code.

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

## Code Spacing and formatting

- Segment code into logical stanzas separated by newlines.

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

- Use spacing between `case` and `select` stanzas.

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

### 80 character line length

- Wrap columns at 80 characters.
- Tabs are 8 spaces.

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

- If a function call exceeds the column limit, place the closing parenthesis
  on its own line and start all arguments on a new line after the opening
  parenthesis.

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

- Compact form is acceptable if visual symmetry of parentheses is preserved.

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

### Exception for log and error message formatting

- Minimize lines for log and error messages, while adhering to the
  80-character limit.

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

### Exceptions and additional styling for structured logging

- **Static messages:** Use key-value pairs instead of formatted strings for the
  `msg` parameter.
- **Key-value attributes:** Use `slog.Attr` helper functions.
- **Line wrapping:** Structured log lines are an exception to the 80-character
  rule. Use one line per key-value pair for multiple attributes.

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

### Wrapping long function definitions

- If function arguments exceed the 80-character limit, maintain indentation
  on following lines.
- Do not end a line with an open parenthesis if the function definition is not
  finished.

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

- If a function declaration spans multiple lines, the body should start with an
  empty line.

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

## Use of Log Levels

- Available levels: `trace`, `debug`, `info`, `warn`, `error`, `critical`.
- Only use `error` for internal errors not triggered by external sources.

## Testing

- To run all tests for a specific package:
  `make unit pkg=$pkg`
- To run a specific test case within a package:
  `make unit pkg=$pkg case=$case`

## Git Commit Messages

- **Subject Line:**
  - Format: `subsystem: short description of changes`
  - `subsystem` should be the package primarily affected (e.g., `lnwallet`, `rpcserver`).
  - For multiple packages, use `+` or `,` as a delimiter (e.g., `lnwallet+htlcswitch`).
  - For widespread changes, use `multi:`.
  - Keep it under 50 characters.
  - Use the present tense (e.g., "Fix bug", not "Fixed bug").

- **Message Body:**
  - Separate from the subject with a blank line.
  - Explain the "what" and "why" of the change.
  - Wrap text to 72 characters.
  - Use bullet points for lists.
