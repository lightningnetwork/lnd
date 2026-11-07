//go:build test_db_postgres && !js && !(windows && (arm || 386)) && !(linux && (ppc64 || mips || mipsle || mips64)) && !(netbsd || openbsd)

package sqldb

import "strings"

// sanitizeDockerName returns a Docker-safe container name.
func sanitizeDockerName(name string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r

		case r >= 'A' && r <= 'Z':
			return r

		case r >= '0' && r <= '9':
			return r

		case r == '_', r == '-':
			return r

		default:
			return '_'
		}
	}, name)

	sanitized = strings.Trim(sanitized, "_.-")
	if sanitized == "" {
		return "postgresql-container"
	}

	return sanitized
}
