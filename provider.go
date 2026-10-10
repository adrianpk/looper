package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

func (a *app) providerAccess() error {
	c, _, err := hostContext()
	if err != nil {
		return err
	}
	_, err = accessRead("provider", func(ctx context.Context) (string, error) {
		if c.ProviderProbe == "" {
			return "", codexAccess(ctx, a.config.Repo)
		}
		child, err := executionCommand(ctx, a.config.Repo, c.ProviderProbe)
		if err != nil {
			return "", err
		}
		child.WaitDelay = 2 * time.Second
		// Probe output is deliberately discarded, including on failure.
		if err := child.Run(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				class := map[int]string{75: "transient", 77: "authentication", 78: "configuration"}[exit.ExitCode()]
				if class == "" {
					class = "access"
				}
				return "", contextFailure("provider", class, "Host provider probe did not confirm access", 1)
			}
			return "", err
		}
		return "", nil
	})
	return err
}

// This client never starts or resumes a thread. All account data stays in memory.
func codexAccess(ctx context.Context, dir string) (result error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c, err := executionCommand(ctx, dir, "codex", "app-server", "--listen", "stdio://")
	if err != nil {
		return err
	}
	stdin, err := c.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	c.Stderr = io.Discard
	if err := c.Start(); err != nil {
		return err
	}
	defer func() {
		_ = stdin.Close()
		cancel() // Own, read-only probe only; never touches a delivery worker or daemon.
		_ = c.Wait()
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	request := func(id int, method string, params any) (json.RawMessage, error) {
		if err := json.NewEncoder(stdin).Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for range 128 {
			if !scanner.Scan() {
				return nil, errors.New("provider probe closed without a response")
			}
			var response struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
				return nil, errors.New("invalid provider probe response")
			}
			if response.ID != id {
				continue
			}
			if response.Error != nil {
				return nil, fmt.Errorf("provider refused access: %s", response.Error.Message)
			}
			return response.Result, nil
		}
		return nil, errors.New("provider probe notification limit exceeded")
	}
	if _, err := request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "looper-preflight", "title": "Looper access check", "version": version}}); err != nil {
		return err
	}
	if err := json.NewEncoder(stdin).Encode(map[string]string{"method": "initialized"}); err != nil {
		return err
	}
	raw, err := request(2, "account/read", map[string]bool{"refreshToken": false})
	if err != nil {
		return err
	}
	var auth struct {
		Account *struct {
			Type string `json:"type"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		return err
	}
	if auth.Account == nil || auth.Account.Type != "chatgpt" {
		if auth.Account == nil {
			return contextFailure("provider", "authentication", "No authenticated provider account; restore host login or configure a host provider_probe", 1)
		}
		return contextFailure("provider", "configuration", "Configure a host provider_probe for providers without ChatGPT account access", 1)
	}
	raw, err = request(3, "account/rateLimits/read", nil)
	if err != nil {
		return err
	}
	type window struct {
		UsedPercent float64 `json:"usedPercent"`
	}
	type bucket struct {
		Primary   *window `json:"primary"`
		Secondary *window `json:"secondary"`
		Reached   *string `json:"rateLimitReachedType"`
	}
	var limits struct {
		RateLimits *bucket `json:"rateLimits"`
	}
	if err := json.Unmarshal(raw, &limits); err != nil || limits.RateLimits == nil {
		return contextFailure("provider", "access", "Provider returned no usable quota information", 1)
	}
	b := limits.RateLimits
	if b.Reached != nil && *b.Reached != "" || b.Primary != nil && b.Primary.UsedPercent >= 100 || b.Secondary != nil && b.Secondary.UsedPercent >= 100 {
		return contextFailure("provider", "resource", "Provider quota is exhausted; wait for reset or restore account capacity", 1)
	}
	return nil
}
