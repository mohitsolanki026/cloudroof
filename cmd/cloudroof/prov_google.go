//go:build !nogcp

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package main: Google Cloud adapter registration (compile out with -tags nogcp).
package main

import _ "github.com/mohitsolanki026/cloudroof/internal/provider/google"
