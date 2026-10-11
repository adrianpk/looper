package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Only host locations belong in this descriptor. Authentication stays with its owner.
type executionContext struct {
	Version       int               `json:"version"`
	Environment   map[string]string `json:"environment,omitempty"`
	ProviderProbe string            `json:"provider_probe,omitempty"`
	Executables   []string          `json:"required_executables,omitempty"`
}

var contextKeys = []string{"HOME", "PATH", "CODEX_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "SSH_AUTH_SOCK", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "HERDR_CONFIG_PATH", "NO_COLOR"}

func hostContext() (executionContext, string, error) {
	path := os.Getenv("LOOPER_CONTEXT")
	explicit := path != ""
	if !explicit {
		home, err := os.UserHomeDir()
		if err != nil {
			return executionContext{}, "", err
		}
		path = filepath.Join(home, ".config", "looper", "context.json")
	}
	var c executionContext
	err := readJSON(path, &c)
	if !explicit && errors.Is(err, os.ErrNotExist) {
		return executionContext{Version: 1}, "", nil
	}
	if err != nil || !filepath.IsAbs(path) || c.Version != 1 {
		return c, path, contextFailure("context", "configuration", "Host context is missing or invalid", 1)
	}
	allowed := map[string]bool{}
	for _, key := range contextKeys {
		allowed[key] = true
	}
	for key, value := range c.Environment {
		if !allowed[key] || len(value) > 8192 || strings.ContainsAny(value, "\x00\r\n") {
			return c, path, contextFailure("context", "configuration", "Host context contains an unsupported environment override", 1)
		}
		if value != "" && key != "PATH" && key != "NO_COLOR" && !filepath.IsAbs(value) {
			return c, path, contextFailure("context", "configuration", "Host context locations must be absolute", 1)
		}
		if key == "PATH" {
			for _, dir := range filepath.SplitList(value) {
				if !filepath.IsAbs(dir) {
					return c, path, contextFailure("context", "configuration", "Host PATH entries must be absolute", 1)
				}
			}
		}
	}
	if c.ProviderProbe != "" && !filepath.IsAbs(c.ProviderProbe) {
		return c, path, contextFailure("provider", "configuration", "Provider probe must be an absolute host executable", 1)
	}
	if len(c.Executables) > 64 {
		return c, path, contextFailure("context", "configuration", "At most 64 extra executables can be required", 1)
	}
	for _, name := range c.Executables {
		if name == "" || len(name) > 4096 || strings.ContainsAny(name, "\x00\r\n \t") || strings.ContainsRune(name, filepath.Separator) && !filepath.IsAbs(name) {
			return c, path, contextFailure("context", "configuration", "Invalid required executable", 1)
		}
	}
	return c, path, nil
}

func executionEnvironment() ([]string, error) {
	c, path, err := hostContext()
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, item := range terminalEnvironment() {
		key, value, _ := strings.Cut(item, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			continue
		}
		values[key] = value
	}
	for key, value := range c.Environment {
		values[key] = value
	}
	if path != "" {
		values["LOOPER_CONTEXT"] = path
	}
	// Access probes and controller Git operations must not wait for interactive credentials.
	values["GIT_TERMINAL_PROMPT"] = "0"
	if values["GIT_SSH_COMMAND"] == "" {
		// Gates can narrow PATH; retain the transport without making SSH a required tool.
		ssh := "ssh"
		if path, err := resolveExecutable("ssh", []string{"PATH=" + values["PATH"]}); err == nil {
			ssh = shellQuote(path)
		}
		values["GIT_SSH_COMMAND"] = ssh + " -o BatchMode=yes -o ConnectTimeout=10"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}

func environmentValue(env []string, key string) string {
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, key+"="); ok {
			return value
		}
	}
	return ""
}

func resolveExecutable(name string, env []string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		return exec.LookPath(name)
	}
	for _, dir := range filepath.SplitList(environmentValue(env, "PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		if path, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return path, nil
		}
	}
	return "", contextFailure(name, "configuration", "Required executable cannot be resolved in the execution context", 1)
}

func executionCommand(ctx context.Context, dir, name string, args ...string) (*exec.Cmd, error) {
	env, err := executionEnvironment()
	if err != nil {
		return nil, err
	}
	path, err := resolveExecutable(name, env)
	if err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, path, args...)
	c.Dir, c.Env = dir, env
	return c, nil
}

// Forward location references, including empty values, to defeat stale terminal-server state.
// Secret environment values are never sent through terminal command arguments.
func launchEnvironment() ([]string, error) {
	env, err := executionEnvironment()
	if err != nil {
		return nil, err
	}
	var result []string
	for _, key := range append(append([]string{}, contextKeys...), "LOOPER_CONTEXT") {
		result = append(result, key+"="+environmentValue(env, key))
	}
	return result, nil
}

type executionFailure struct{ Blocker runBlocker }

func (e *executionFailure) Error() string {
	return fmt.Sprintf("%s: %s (%s)", e.Blocker.Component, e.Blocker.Message, e.Blocker.Class)
}

func contextFailure(component, class, reason string, attempts int) *executionFailure {
	action := "restore_access_then_resume"
	switch class {
	case "configuration":
		action = "repair_host_context_then_resume"
	case "authentication":
		action = "restore_host_authentication_then_resume"
	case "validation":
		action = "inspect_validation_then_resume"
	case "ownership":
		action = "inspect_owner_before_resuming"
	case "transient":
		action = "restore_service_then_resume"
	}
	return &executionFailure{Blocker: runBlocker{Code: component + "_" + class, Component: component, Class: class, Message: reason, Attempts: attempts, Retryable: class != "ownership", NextAction: inspectAction(action, component)}}
}

// Diagnose in memory, then retain only a fixed safe reason, never remote stderr or credentials.
func classifyAccess(component string, err error, attempts int) *executionFailure {
	var known *executionFailure
	if errors.As(err, &known) {
		copy := *known
		copy.Blocker.Attempts = attempts
		return &copy
	}
	message := strings.ToLower(err.Error())
	class, reason := "access", "Access could not be verified; inspect the host tool outside Looper"
	for _, pattern := range []string{"permission denied", "authentication", "unauthorized", "forbidden", "401", "403", "could not read username", "agent refused", "no identities", "not logged in", "expired", "invalid token"} {
		if strings.Contains(message, pattern) {
			class, reason = "authentication", "Authentication is unavailable or was rejected; restore host credentials"
			break
		}
	}
	if class == "access" {
		for _, pattern := range []string{"timed out", "timeout", "deadline exceeded", "connection reset", "connection refused", "temporary failure", "could not resolve", "network is unreachable", "429", "502", "503", "504"} {
			if strings.Contains(message, pattern) {
				class, reason = "transient", "Remote access remains unavailable after bounded retries"
				break
			}
		}
	}
	return contextFailure(component, class, reason, attempts)
}

func accessRead(component string, operation func(context.Context) (string, error)) (string, error) {
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, err := operation(ctx)
		deadline := ctx.Err()
		cancel()
		if err == nil {
			return out, nil
		}
		if deadline != nil {
			err = deadline
		}
		failure := classifyAccess(component, err, attempt)
		if failure.Blocker.Class != "transient" || attempt == 3 {
			return "", failure
		}
		time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
	}
	panic("unreachable retry count")
}

func accessCommand(component, dir, name string, args ...string) (string, error) {
	return accessRead(component, func(ctx context.Context) (string, error) {
		c, err := executionCommand(ctx, dir, name, args...)
		if err != nil {
			return "", err
		}
		c.WaitDelay = 2 * time.Second
		return commandOutput(c)
	})
}

func (a *app) preflight() error {
	env, err := executionEnvironment()
	if err != nil {
		return err
	}
	c, _, err := hostContext()
	if err != nil {
		return err
	}
	for _, name := range append([]string{"git", "tea", "codex", "acta", "sh"}, c.Executables...) {
		if _, err := resolveExecutable(name, env); err != nil {
			return err
		}
	}
	origin, err := git(a.config.Repo, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if origin != a.config.Origin {
		return contextFailure("git", "configuration", "Repository origin differs from the approved configuration", 1)
	}
	if _, err := accessCommand("git", a.config.Repo, "git", "ls-remote", "--exit-code", "origin", "refs/heads/dev"); err != nil {
		return err
	}
	var user struct {
		ID int `json:"id"`
	}
	if err := a.api("GET", "user", nil, &user); err != nil {
		return err
	}
	if user.ID < 1 {
		return contextFailure("forge", "authentication", "Forgejo did not confirm an authenticated identity", 1)
	}
	var repo struct {
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
		AllowRebase bool `json:"allow_rebase"`
	}
	if err := a.api("GET", a.endpoint(""), nil, &repo); err != nil {
		return err
	}
	if !repo.Permissions.Push || !repo.AllowRebase {
		return contextFailure("forge", "configuration", "Repository requires push permission and Rebase + Fast-forward integration", 1)
	}
	return a.providerAccess()
}

func workerHome() (string, error) {
	env, err := executionEnvironment()
	if err != nil {
		return "", err
	}
	home := environmentValue(env, "CODEX_HOME")
	if home == "" {
		home = filepath.Join(environmentValue(env, "HOME"), ".codex")
	}
	if !filepath.IsAbs(home) {
		return "", contextFailure("context", "configuration", "Worker credential and conversation home must be absolute", 1)
	}
	return filepath.Clean(home), nil
}
