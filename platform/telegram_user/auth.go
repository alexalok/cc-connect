package telegram_user

import (
	"context"
	"fmt"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
)

// Login authorizes an existing user account and persists its session. Daemon
// startup only reuses this session; it never prompts or requests a login code.
func Login(ctx context.Context, opts map[string]any, authenticator auth.UserAuthenticator) (int64, error) {
	c, err := parseCredentials(opts)
	if err != nil {
		return 0, err
	}
	client := telegram.NewClient(c.appID, c.appHash, c.clientOptions())
	var userID int64
	err = client.Run(ctx, func(ctx context.Context) error {
		if err := client.Auth().IfNecessary(ctx, auth.NewFlow(authenticator, auth.SendCodeOptions{})); err != nil {
			return fmt.Errorf("telegram_user: authenticate: %w", err)
		}
		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("telegram_user: get account: %w", err)
		}
		if self.Bot {
			return fmt.Errorf("telegram_user: session belongs to a bot; use a regular user account")
		}
		userID = self.ID
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("telegram_user: login: %w", err)
	}
	return userID, nil
}
