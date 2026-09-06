//go:build !nodo

// Package main: DigitalOcean adapter registration (compile out with -tags nodo).
package main

import _ "bosun/internal/provider/digitalocean"
