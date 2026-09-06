//go:build !nohetzner

// Package main: Hetzner adapter registration (compile out with -tags nohetzner).
package main

import _ "bosun/internal/provider/hetzner"
