package pricing

import "fmt"

func fmtSscanf(raw string, format string, v *float64) (int, error) {
	return fmt.Sscanf(raw, format, v)
}
