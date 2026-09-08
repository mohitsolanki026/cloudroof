//go:build !novultr

// Package main: Vultr adapter registration (compile out with -tags novultr).
package main

import _ "cloudroof/internal/provider/vultr"
