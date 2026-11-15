package accounts

import "os"

// getenv is a seam so tests can inject environment values.
var getenv = os.Getenv

// SetGetenv replaces the environment reader (tests only).
func SetGetenv(fn func(string) string) {
	if fn != nil {
		getenv = fn
	}
}
