//go:build !no_telegram_user

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/platform/telegram_user"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"
)

func runTelegramUser(args []string) {
	if err := telegramUserLogin(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func telegramUserLogin(args []string) error {
	i18n := core.NewI18n(core.LangEnglish)
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(i18n.T(core.MsgTelegramUserLoginUsage))
		return nil
	}
	if args[0] != "login" {
		return fmt.Errorf("%s", i18n.T(core.MsgTelegramUserLoginUsage))
	}
	fs := flag.NewFlagSet("telegram-user login", flag.ContinueOnError)
	configFile := fs.String("config", "", "")
	project := fs.String("project", "", "")
	fs.Usage = func() { fmt.Fprintln(fs.Output(), i18n.T(core.MsgTelegramUserLoginUsage)) }
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%s", i18n.T(core.MsgTelegramUserLoginUsage))
	}
	cfg, err := config.LoadPermissive(resolveConfigPath(*configFile))
	if err != nil {
		return fmt.Errorf("telegram_user: load config: %w", err)
	}
	i18n = core.NewI18n(core.NormalizeLanguageString(cfg.Language))
	opts, err := telegramUserOptions(cfg, *project)
	if err != nil {
		return err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("%s", i18n.T(core.MsgTelegramUserTerminalRequired))
	}
	state, err := term.GetState(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("telegram_user: read terminal state: %w", err)
	}
	defer func() {
		if err := term.Restore(int(os.Stdin.Fd()), state); err != nil {
			slog.Warn("telegram_user: restore terminal", "error", err)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	uid, err := telegram_user.Login(ctx, opts, &telegramUserTerminal{i18n: i18n})
	if err != nil {
		hash, _ := opts["api_hash"].(string)
		return errors.New(core.RedactToken(err.Error(), hash))
	}
	fmt.Println(i18n.Tf(core.MsgTelegramUserLoginSuccess, uid))
	return nil
}

func telegramUserOptions(cfg *config.Config, project string) (map[string]any, error) {
	var found []map[string]any
	for _, p := range cfg.Projects {
		if project != "" && p.Name != project {
			continue
		}
		for _, platform := range p.Platforms {
			if platform.Type == "telegram_user" {
				found = append(found, platform.Options)
			}
		}
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("telegram_user: expected exactly one telegram_user platform, found %d; select its project with --project", len(found))
	}
	return found[0], nil
}

type telegramUserTerminal struct{ i18n *core.I18n }

func (t *telegramUserTerminal) read(ctx context.Context, key core.MsgKey, trim bool) (string, error) {
	fmt.Fprint(os.Stderr, t.i18n.T(key))
	type result struct {
		value string
		err   error
	}
	ready := make(chan result, 1)
	go func() {
		data, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		value := string(data)
		clear(data)
		if trim {
			value = strings.TrimSpace(value)
		}
		ready <- result{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ready:
		return r.value, r.err
	}
}

func (t *telegramUserTerminal) Phone(ctx context.Context) (string, error) {
	return t.read(ctx, core.MsgTelegramUserPhone, true)
}

func (t *telegramUserTerminal) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	return t.read(ctx, core.MsgTelegramUserCode, true)
}

func (t *telegramUserTerminal) Password(ctx context.Context) (string, error) {
	return t.read(ctx, core.MsgTelegramUserPassword, false)
}

func (t *telegramUserTerminal) AcceptTermsOfService(context.Context, tg.HelpTermsOfService) error {
	return errors.New(t.i18n.T(core.MsgTelegramUserExistingAccount))
}

func (t *telegramUserTerminal) SignUp(context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New(t.i18n.T(core.MsgTelegramUserExistingAccount))
}
