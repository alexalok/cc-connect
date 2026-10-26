//go:build no_telegram_user

package main

import (
	"fmt"
	"os"

	"github.com/chenhg5/cc-connect/core"
)

func runTelegramUser([]string) {
	fmt.Fprintln(os.Stderr, core.NewI18n(core.LangEnglish).T(core.MsgTelegramUserDisabled))
	os.Exit(1)
}
