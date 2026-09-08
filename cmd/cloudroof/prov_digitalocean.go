//go:build !nodo

// Package main: DigitalOcean adapter registration (compile out with -tags nodo).
package main

import _ "cloudroof/internal/provider/digitalocean"
