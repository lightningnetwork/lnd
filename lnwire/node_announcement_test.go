package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNodeAliasValidation tests that the NewNodeAlias method will only accept
// valid node announcements.
func TestNodeAliasValidation(t *testing.T) {
	t.Parallel()

	var testCases = []struct {
		alias string
		valid bool
	}{
		// UTF-8 alias with valid length.
		{
			alias: "meruem",
			valid: true,
		},

		// UTF-8 alias with invalid length.
		{
			alias: "p3kysxqr23swl33m6h5grmzddgw5nsgkky3g52zc6frpwz",
			valid: false,
		},

		// String with non UTF-8 characters.
		{
			alias: "\xE0\x80\x80",
			valid: false,
		},
	}

	for _, testCase := range aliasTextCases() {
		testCases = append(testCases, struct {
			alias string
			valid bool
		}{alias: testCase.text, valid: testCase.valid})
	}
	for _, testCase := range testCases {
		_, err := NewNodeAlias(testCase.alias)
		if testCase.valid {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}

	_, err := NewNodeAlias("👨‍💻")
	require.ErrorContains(t, err, "disallowed Unicode character U+200D")

	// Readers can preserve an unassigned code point in a received alias.
	const unassigned = "node \u0378"
	_, err = NewNodeAlias(unassigned)
	require.ErrorAs(t, err, new(*ErrInvalidNodeAlias))
	alias, err := ParseNodeAlias(unassigned)
	require.NoError(t, err)
	require.Equal(t, unassigned, alias.String())
}

// aliasTextCases covers the BOLT 1 Unicode rules for every UTF-8 text field.
func aliasTextCases() []struct {
	name  string
	text  string
	valid bool
} {

	return []struct {
		name  string
		text  string
		valid bool
	}{
		{name: "Cc control", text: "meruem\t"},
		{name: "Cf format", text: "meruem\u200b"},
		{name: "Co private use", text: "meruem\ue000"},
		{name: "Zl line separator", text: "meruem\u2028"},
		{name: "Zp paragraph separator", text: "meruem\u2029"},
		{name: "embedded NUL", text: "mer\x00uem"},
		{name: "malformed UTF-8", text: "meruem\xff"},
		{name: "Japanese", text: "ナンセンス 1杯", valid: true},
		{name: "emoji", text: "node \U0001f369", valid: true},
		{name: "combining mark", text: "cafe\u0301", valid: true},
	}
}

// TestNodeAliasWirePadding verifies that only trailing NUL padding is ignored
// during validation and that the original 32 wire bytes are preserved.
func TestNodeAliasWirePadding(t *testing.T) {
	t.Parallel()

	var original NodeAlias
	copy(original[:], "meruem")

	var buf bytes.Buffer
	require.NoError(t, WriteElement(&buf, original))
	require.Equal(t, original[:], buf.Bytes())

	var decoded NodeAlias
	require.NoError(t, ReadElement(bytes.NewReader(buf.Bytes()), &decoded))
	require.Equal(t, original, decoded)

	var embedded NodeAlias
	copy(embedded[:], "mer\x00uem")

	buf.Reset()
	var invalidAlias *ErrInvalidNodeAlias
	require.ErrorAs(t, WriteElement(&buf, embedded), &invalidAlias)
	require.ErrorAs(
		t, ReadElement(bytes.NewReader(embedded[:]), &decoded),
		&invalidAlias,
	)

	// The constructor sees the complete unpadded input, so a trailing NUL
	// supplied by a local caller is invalid too.
	_, err := NewNodeAlias("meruem\x00")
	require.ErrorAs(t, err, &invalidAlias)
}
