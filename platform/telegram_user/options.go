package telegram_user

import (
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"golang.org/x/net/proxy"
)

type credentials struct {
	appID       int
	appHash     string
	sessionFile string
	resolver    dcs.Resolver
}

func parseCredentials(opts map[string]any) (credentials, error) {
	var c credentials
	id, err := strconv.ParseInt(fmt.Sprint(opts["api_id"]), 10, 32)
	if err != nil || id <= 0 || id > math.MaxInt32 {
		return c, fmt.Errorf("telegram_user: api_id must be a positive integer")
	}
	c.appID = int(id)
	c.appHash, _ = opts["api_hash"].(string)
	if hash, err := hex.DecodeString(c.appHash); err != nil || len(hash) != 16 {
		return c, fmt.Errorf("telegram_user: api_hash must contain 32 hexadecimal characters")
	}
	c.sessionFile, _ = opts["session_file"].(string)
	if strings.TrimSpace(c.sessionFile) == "" {
		return c, fmt.Errorf("telegram_user: session_file is required")
	}
	if strings.HasPrefix(c.sessionFile, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return c, fmt.Errorf("telegram_user: home directory: %w", err)
		}
		c.sessionFile = filepath.Join(homeDir, c.sessionFile[2:])
	}
	if !filepath.IsAbs(c.sessionFile) {
		return c, fmt.Errorf("telegram_user: session_file must be an absolute path (or start with ~/) so login and daemon use the same session")
	}
	if raw, _ := opts["proxy"].(string); raw != "" {
		c.resolver, err = proxyResolver(raw)
		if err != nil {
			return c, err
		}
	}
	return c, nil
}

func proxyResolver(raw string) (dcs.Resolver, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "socks5" || u.Hostname() == "" || u.Port() == "" {
		return nil, fmt.Errorf("telegram_user: proxy must be a socks5://host:port URL")
	}
	dialer, err := proxy.FromURL(u, &net.Dialer{Timeout: 15 * time.Second})
	if err != nil {
		// Do not include the URL, which can contain a proxy password.
		return nil, fmt.Errorf("telegram_user: cannot configure SOCKS5 proxy")
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("telegram_user: proxy does not support cancellation")
	}
	return dcs.Plain(dcs.PlainOptions{Dial: contextDialer.DialContext}), nil
}

func (c credentials) clientOptions() telegram.Options {
	return telegram.Options{
		SessionStorage: &sessionStorage{path: c.sessionFile},
		Resolver:       c.resolver,
		Device: telegram.DeviceConfig{
			DeviceModel: "CC-Connect", AppVersion: "1.0", SystemLangCode: "en", LangCode: "en",
		},
	}
}

func validateIDList(name, value string, required bool, positive bool) error {
	if value == "*" || (value == "" && !required) {
		return nil
	}
	if value == "" {
		return fmt.Errorf("telegram_user: %s is required; set explicit numeric IDs or *", name)
	}
	for _, item := range strings.Split(value, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(item), 10, 64)
		if err != nil || id == 0 || (positive && id < 0) {
			return fmt.Errorf("telegram_user: %s must contain comma-separated numeric IDs or *", name)
		}
	}
	return nil
}
