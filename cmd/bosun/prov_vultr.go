//go:build !novultr

// Package main: Vultr adapter registration (compile out with -tags novultr).
package main

import _ "bosun/internal/provider/vultr"
