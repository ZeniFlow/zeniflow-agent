//go:build !linux

package main

import "context"

// applyFirewall: правила применяются только на Linux.
func applyFirewall(context.Context) {}
