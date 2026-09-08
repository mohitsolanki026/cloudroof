//go:build !noazure

// Package main: Azure adapter registration (compile out with -tags noazure).
package main

import _ "cloudroof/internal/provider/azure"
