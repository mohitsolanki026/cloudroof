// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package web embeds the built frontend so the final binary is self-contained.
//
// `make web` populates dist/. The all: prefix includes dotfiles, which lets a
// .gitkeep satisfy the embed directive on a fresh checkout before the first
// build.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist is the bundle rooted at dist/ (index.html at the top level).
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
