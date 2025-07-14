package sqlc

import (
	"fmt"
	"strings"
)

// makeQueryParams generates a string of query parameters for a SQL query. It is
// meant to replace the `?` placeholders in a SQL query with numbered parameters
// like `$1`, `$2`, etc. This is required for the sqlc /*SLICE:<field_name>*/
// workaround. See scripts/gen_sqlc_docker.sh for more details.
func makeQueryParams(numTotalArgs, numListArgs int) (string, error) {
	if numListArgs == 0 {
		return "", nil
	}

	var b strings.Builder

	// Pre-allocate a rough estimation of the buffer size to avoid
	// re-allocations. A parameter like $1000, takes 6 bytes.
	b.Grow(numListArgs * 6)

	diff := numTotalArgs - numListArgs
	for i := 0; i < numListArgs; i++ {
		if i > 0 {
			_, err := b.WriteString(",")
			if err != nil {
				return "", err
			}
		}
		_, err := fmt.Fprintf(&b, "$%d", i+diff+1)
		if err != nil {
			return "", err
		}
	}

	return b.String(), nil
}
