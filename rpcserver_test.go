package lnd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetAllPermissions(t *testing.T) {
	perms := GetAllPermissions()

	// Currently there are there are 16 entity:action pairs in use.
	assert.Equal(t, len(perms), 16)
}
