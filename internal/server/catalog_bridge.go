package server

import "github.com/xzs/bridge_qoder/internal/catalog"

func catalogSupportedEfforts(meta catalog.Model) []string { return catalog.SupportedEfforts(meta) }
func catalogDefaultEffort(meta catalog.Model) string      { return catalog.DefaultEffort(meta) }
