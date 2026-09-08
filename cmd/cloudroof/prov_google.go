//go:build !nogcp

// Package main: Google Cloud adapter registration (compile out with -tags nogcp).
package main

import _ "cloudroof/internal/provider/google"
