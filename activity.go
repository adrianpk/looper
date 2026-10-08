package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unsafe"
)

// Activity is display-only. It never writes controller state or grants authority.
type activityMonitor struct {
	mu        sync.Mutex
	output    *terminalOutput
	enabled   bool
	active    bool
	label     string
	lastEvent time.Time
	latest    string
	tools     map[string]string
}

type activityStyle string

const (
	activityMessage activityStyle = "\x1b[34m"
	activityRunning activityStyle = "\x1b[36m"
	activitySuccess activityStyle = "\x1b[32m"
	activityWarning activityStyle = "\x1b[33m"
	activityFailure activityStyle = "\x1b[31m"
	activityQuiet   activityStyle = "\x1b[2m"
)

func activityTerminal(out io.Writer) bool {
	if value, _ := os.LookupEnv("NO_COLOR"); value != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	var termios syscall.Termios
	// TCGETS distinguishes a terminal from pipes, regular files and other devices.
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

var commandActivity = regexp.MustCompile(`\b(go (?:test|vet|build|fmt)|git (?:status|diff|commit|push|fetch|worktree)|make|python3|node|npm|psql|pg_ctl|initdb|(?:looper|slice-loop) validate)\b`)
var activityIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,80}$`)
var inlineActivityCode = regexp.MustCompile("`[^`]*`")
var activitySecret = regexp.MustCompile(`(?i)\b(password|passwd|token|secret|api[_-]?key|authorization)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
var activityBearer = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;]+`)

func activityText(text string) string {
	if len(text) > 8192 {
		text = text[:8192]
	}
	if json.Valid([]byte(text)) {
		return ""
	}
	var lines []string
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			continue
		}
		if trimmed == "" && len(lines) > 0 {
			break
		}
		if trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	text = inlineActivityCode.ReplaceAllString(strings.Join(lines, " "), "[code]")
	text = activityBearer.ReplaceAllString(text, "Bearer [redacted]")
	text = activitySecret.ReplaceAllString(text, "$1=[redacted]")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	runes := []rune(strings.ToValidUTF8(text, ""))
	if len(runes) > 360 {
		text = string(runes[:360]) + "…"
	}
	return text
}

func (m *activityMonitor) printLocked(now time.Time, kind, text string, style activityStyle) {
	if m.enabled && text != "" {
		m.output.emit(now, kind, m.label, text, style)
	}
}

func (m *activityMonitor) begin(label, kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.label, m.active, m.lastEvent = label, true, time.Now()
	m.latest = "waiting for events"
	m.tools = make(map[string]string)
	m.printLocked(m.lastEvent, kind, "Started", activityRunning)
}

func (m *activityMonitor) end() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = false
	m.tools = nil
}

func (m *activityMonitor) event(raw []byte) {
	var event struct {
		Type string `json:"type"`
		Item struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Text     string `json:"text"`
			Command  string `json:"command"`
			Tool     string `json:"tool"`
			Server   string `json:"server"`
			Status   string `json:"status"`
			ExitCode *int   `json:"exit_code"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &event) != nil || event.Type == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return
	}
	now := time.Now()
	m.lastEvent = now
	if event.Type == "error" || event.Type == "turn.failed" {
		m.latest = "Worker reported an error"
		m.printLocked(now, "worker.error", m.latest, activityFailure)
		return
	}
	if event.Type != "item.started" && event.Type != "item.completed" {
		return
	}
	item := event.Item
	if item.Type == "agent_message" && event.Type == "item.completed" {
		if text := activityText(item.Text); text != "" {
			m.latest = text
			m.printLocked(now, "worker.message", "Worker: "+text, activityMessage)
		}
		return
	}
	var name string
	switch item.Type {
	case "command_execution":
		name = activityCommand(item.Command)
	case "mcp_tool_call":
		name = "tool"
		if activityIdentifier.MatchString(item.Tool) {
			name += " " + item.Tool
		}
		if activityIdentifier.MatchString(item.Server) {
			name += " (" + item.Server + ")"
		}
	case "file_change":
		name = "file changes"
	case "web_search":
		name = "web search"
	default:
		return // Reasoning, payloads and unknown items never become display text.
	}
	style := activityRunning
	kind := "tool.started"
	if event.Type == "item.started" {
		// Bound metadata even if a malformed stream never completes its items.
		if len(m.tools) < 64 && len(item.ID) <= 128 && item.ID != "" {
			m.tools[item.ID] = name
		}
		m.latest = "Started: " + name
	} else {
		kind = "tool.completed"
		style = activitySuccess
		delete(m.tools, item.ID)
		m.latest = "Completed: " + name
		if item.Status == "failed" || item.ExitCode != nil && *item.ExitCode != 0 {
			kind = "tool.failed"
			m.latest = "Failed: " + name
			style = activityFailure
		}
		if item.ExitCode != nil {
			m.latest += fmt.Sprintf(" (exit %d)", *item.ExitCode)
		}
	}
	m.printLocked(now, kind, m.latest, style)
}

func activityCommand(command string) string {
	if name := commandActivity.FindString(command); name != "" {
		return name
	}
	return "shell command"
}

func (m *activityMonitor) check(number, total int, command string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastEvent = time.Now()
	m.latest = fmt.Sprintf("Check %d/%d: %s", number, total, activityCommand(command))
	m.printLocked(m.lastEvent, "validation.check", m.latest, activityRunning)
}

func (m *activityMonitor) heartbeat(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return
	}
	idle := max(time.Duration(0), now.Sub(m.lastEvent)).Truncate(time.Second)
	text := fmt.Sprintf("Last activity event %s ago; tracked tools in progress: %d; latest: %s", idle, len(m.tools), m.latest)
	style := activityQuiet
	if idle >= 5*time.Minute {
		style = activityWarning
		text = "No activity events for " + idle.String() + "; a command or request may still be running. No interruption. Latest: " + m.latest
	}
	m.printLocked(now, "heartbeat", text, style)
}

func (m *activityMonitor) control(mode string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch mode {
	case "color:auto", "color:always", "color:never":
		color := strings.TrimPrefix(mode, "color:")
		m.output.setColor(colorMode(color))
		return "Color: " + color
	case "on":
		m.enabled = true
	case "off":
		m.printLocked(time.Now(), "activity.control", "Activity disabled", activityRunning)
		m.enabled = false
	case "toggle":
		if m.enabled {
			m.printLocked(time.Now(), "activity.control", "Activity disabled", activityRunning)
		}
		m.enabled = !m.enabled
	case "status":
	default:
		return "error: unknown activity command"
	}
	if m.enabled {
		if mode != "status" {
			m.printLocked(time.Now(), "activity.control", "Activity enabled; latest: "+m.latest, activityRunning)
		}
		return "Activity: on"
	}
	return "Activity: off"
}

// Linux abstract sockets avoid path-length limits and leave no stale control file.
func activityAddress(configPath string) (string, error) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return "", err
	}
	// Keep the existing control address so Looper can control visibility on older active controllers.
	return fmt.Sprintf("@slice-loop-activity-%d-%s", os.Getuid(), digest([]byte(abs))[:24]), nil
}

func sameActivityUser(conn *net.UnixConn) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var peer *syscall.Ucred
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		peer, peerErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	return err == nil && peerErr == nil && peer != nil && peer.Uid == uint32(os.Getuid())
}

func startActivity(configPath string, enabled bool, output *terminalOutput) (*activityMonitor, func(), error) {
	address, err := activityAddress(configPath)
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		return nil, nil, fmt.Errorf("start activity control: %w", err)
	}
	m := &activityMonitor{output: output, enabled: enabled, label: "Controller"}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if sameActivityUser(conn) {
				request, err := bufio.NewReader(io.LimitReader(conn, 16)).ReadString('\n')
				if err == nil {
					_, _ = fmt.Fprintln(conn, m.control(strings.TrimSpace(request)))
				}
			}
			_ = conn.Close()
		}
	}()
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				m.heartbeat(now)
			case <-done:
				return
			}
		}
	}()
	stop := func() { close(done); _ = listener.Close(); wg.Wait() }
	return m, stop, nil
}

func activityRequest(configPath, mode string) (string, error) {
	address, err := activityAddress(configPath)
	if err != nil {
		return "", err
	}
	conn, err := net.DialTimeout("unix", address, time.Second)
	if err != nil {
		return "", errors.New("no compatible controller is listening for runtime activity control")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintln(conn, mode); err != nil {
		return "", err
	}
	response, err := bufio.NewReader(io.LimitReader(conn, 128)).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read activity status: %w", err)
	}
	response = strings.TrimSpace(response)
	if strings.HasPrefix(mode, "color:") && response == "error: unknown activity command" {
		return "", errors.New("this controller predates runtime color control; use --color on its next start without interrupting active work")
	}
	if response != "Activity: on" && response != "Activity: off" && response != "Color: auto" && response != "Color: always" && response != "Color: never" {
		return "", errors.New("invalid activity control response")
	}
	return response, nil
}
