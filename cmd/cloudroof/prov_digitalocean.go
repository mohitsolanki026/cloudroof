//go:build !nodo

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package main: DigitalOcean adapter registration (compile out with -tags nodo).
package main

import _ "cloudroof/internal/provider/digitalocean"
