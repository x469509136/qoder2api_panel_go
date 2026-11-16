package server

import "fmt"

func fmtSscanf(raw string, v *int) (int, error) { return fmt.Sscanf(raw, "%d", v) }

func fmtSscanfFloat(raw string, v *float64) (int, error) { return fmt.Sscanf(raw, "%g", v) }
