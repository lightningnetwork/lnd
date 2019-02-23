package main

import "testing"

func TestValidatePassword(t *testing.T) {
	testPasswords := map[string]bool{
		"abc":          false,
		"abc123":       false,
		"testpassword": true,
		"1234567":      false,
		"12345678":     true,
	}

	for password, valid := range testPasswords {
		err := validatePassword([]byte(password))
		if err != nil && valid == true {
			t.Fatalf("expected password %s to be valid but got error %s", password, err)
		}
	}

}
