package lnwire

import "testing"

func TestValidateAlias(t *testing.T) {
	aliasStr := "012345678901234567890"
	alias := NewAlias(aliasStr)
	if err := alias.Validate(); err != nil {
		t.Fatalf("alias was invalid: %v", err)
	}
	if aliasStr != alias.String() {
		t.Fatalf("aliases don't match")
	}
}
