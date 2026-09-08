//go:build !nohetzner

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package main: Hetzner adapter registration (compile out with -tags nohetzner).
package main

import _ "cloudroof/internal/provider/hetzner"
