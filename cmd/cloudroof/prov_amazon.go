//go:build !noaws

// Package main: AWS EC2 adapter registration (compile out with -tags noaws).
package main

import _ "cloudroof/internal/provider/amazon"
