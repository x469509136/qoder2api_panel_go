package keepalive

import "fmt"

func fmtSscanf(raw string, v *int) (int, error) { return fmt.Sscanf(raw, "%d", v) }
