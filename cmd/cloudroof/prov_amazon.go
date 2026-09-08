//go:build !noaws

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package main: AWS EC2 adapter registration (compile out with -tags noaws).
package main

import _ "github.com/mohitsolanki026/cloudroof/internal/provider/amazon"
