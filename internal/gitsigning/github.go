package gitsigning

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type GitHub struct {
	Client  *http.Client
	BaseURL string
	Token   string
}

func (g GitHub) get(ctx context.Context, path string, dst any) error {
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return fmt.Errorf("%w: cannot prepare GitHub request", ErrSetup)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 12 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: GitHub is unavailable; try again", ErrSetup)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: GitHub returned HTTP %d", ErrSetup, res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(dst); err != nil {
		return fmt.Errorf("%w: invalid GitHub response", ErrSetup)
	}
	return nil
}

func (g GitHub) Identity(ctx context.Context, cfg Config) (Config, error) {
	var user struct {
		Login string `json:"login"`
		Name  string `json:"name"`
		ID    int64  `json:"id"`
	}
	if err := g.get(ctx, "/user", &user); err != nil {
		return cfg, err
	}
	if user.Login == "" || user.ID <= 0 || strings.ContainsAny(user.Login, " \t\r\n/@") {
		return cfg, fmt.Errorf("%w: a GitHub user account is required", ErrSetup)
	}
	cfg.Account, cfg.AccountID, cfg.Name = user.Login, user.ID, user.Name
	if strings.TrimSpace(cfg.Name) == "" {
		cfg.Name = user.Login
	}
	cfg.Email = fmt.Sprintf("%d+%s@users.noreply.github.com", user.ID, user.Login)
	return cfg, nil
}

func (g GitHub) Registered(ctx context.Context, cfg Config) (bool, error) {
	expected, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cfg.PublicKey))
	if err != nil {
		return false, fmt.Errorf("%w: saved public key is invalid", ErrSetup)
	}
	for page := 1; ; page++ {
		var keys []struct {
			Key string `json:"key"`
		}
		if err := g.get(ctx, fmt.Sprintf("/users/%s/ssh_signing_keys?per_page=100&page=%d", url.PathEscape(cfg.Account), page), &keys); err != nil {
			return false, err
		}
		for _, item := range keys {
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(item.Key))
			if err == nil && string(key.Marshal()) == string(expected.Marshal()) {
				return true, nil
			}
		}
		if len(keys) < 100 {
			return false, nil
		}
	}
}
