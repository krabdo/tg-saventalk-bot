package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type APIError struct {
	Code        int
	Description string
	Uncertain   bool
	Retry       int
}

func (e *APIError) Error() string { return fmt.Sprintf("Telegram error %d", e.Code) }

type API struct {
	Token, Base, Model, Key string
	TelegramURL             string
	Client                  *http.Client
}

func newAPI(token, base, model, key string) *API {
	return &API{Token: token, Base: base, Model: model, Key: key, TelegramURL: "https://api.telegram.org", Client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}}
}
func (a *API) request(ctx context.Context, url string, body any, timeout time.Duration, auth string, limit int64) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, e := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBufferString(raw(body)))
	if e != nil {
		return nil, 0, errors.New("invalid API URL")
	}
	r.Header.Set("Content-Type", "application/json")
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	res, e := a.Client.Do(r)
	if e != nil {
		return nil, 0, errors.New("API transport failed")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, res.StatusCode, errors.New("API response invalid or too large")
	}
	return b, res.StatusCode, nil
}
func (a *API) telegram(ctx context.Context, method string, body Obj, out any) error {
	timeout := 15 * time.Second
	limit := int64(2_000_000)
	if method == "getUpdates" {
		timeout = 45 * time.Second
		limit = 16_000_000
	}
	b, status, e := a.request(ctx, a.TelegramURL+"/bot"+a.Token+"/"+method, body, timeout, "", limit)
	if e != nil {
		return &APIError{Uncertain: true}
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			Retry int `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(b, &envelope) != nil {
		return &APIError{Code: status, Uncertain: true}
	}
	if !envelope.OK {
		return &APIError{Code: envelope.Code, Description: envelope.Description, Uncertain: status >= 500, Retry: envelope.Parameters.Retry}
	}
	if out != nil {
		if json.Unmarshal(envelope.Result, out) != nil {
			return &APIError{Code: status, Uncertain: true}
		}
	}
	return nil
}
func (a *API) configured() bool {
	u, e := url.Parse(a.Base)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && a.Model != "" && a.Key != ""
}
func (a *API) complete(ctx context.Context, messages []Text) (string, error) {
	if !a.configured() {
		return "", errors.New("AI configuration invalid")
	}
	b, status, e := a.request(ctx, strings.TrimRight(a.Base, "/")+"/chat/completions", Obj{"model": a.Model, "messages": messages, "stream": false}, 45*time.Second, a.Key, 2_000_000)
	if e != nil || status != 200 {
		return "", errors.New("AI request failed")
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &result) != nil || len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return "", errors.New("AI returned no text")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
