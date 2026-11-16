package upstream

import "os"

func defaultEnvLookup(name string) string { return os.Getenv(name) }
