package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal Telegram Bot API client for webhook management.
type Client struct {
	httpClient    *http.Client
	routerBaseURL string // e.g. https://openclaw-router.example.com
}

// New creates a new Telegram client.
// routerBaseURL is the public URL of the Router (no trailing slash).
func New(routerBaseURL string) *Client {
	return &Client{
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		routerBaseURL: strings.TrimRight(routerBaseURL, "/"),
	}
}

type apiResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// RegisterWebhook calls setWebhook for the given bot token and tenant ID.
// The webhook URL will be: {routerBaseURL}/tg/{tenantID}
func (c *Client) RegisterWebhook(ctx context.Context, botToken, tenantID string) error {
	webhookURL := fmt.Sprintf("%s/tg/%s", c.routerBaseURL, tenantID)
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/setWebhook", botToken)

	form := url.Values{}
	form.Set("url", webhookURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("setWebhook: %w", err)
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("telegram error: %s", result.Description)
	}
	return nil
}

// DeleteWebhook removes the webhook for the given bot token.
func (c *Client) DeleteWebhook(ctx context.Context, botToken string) error {
	if botToken == "" {
		return nil // nothing to delete
	}
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/deleteWebhook", botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL,
		strings.NewReader("drop_pending_updates=false"))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("deleteWebhook: %w", err)
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("telegram error: %s", result.Description)
	}
	return nil
}
