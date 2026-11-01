//go:build !linux

package main

import "context"

// watchFirewall: на системах кроме Linux правил нет (разработка); состояние остаётся unsupported.
func watchFirewall(context.Context) {}
