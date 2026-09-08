//go:build !noazure

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package main: Azure adapter registration (compile out with -tags noazure).
package main

import _ "github.com/mohitsolanki026/cloudroof/internal/provider/azure"
