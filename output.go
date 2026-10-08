package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type outputFormat string

const (
	outputJSONL   outputFormat = "jsonl"
	outputHuman   outputFormat = "human"
	defaultOutput              = outputJSONL
)

func validOutput(format outputFormat) bool { return format == outputJSONL || format == outputHuman }

type logEvent struct {
	SchemaVersion int    `json:"schema_version"`
	Time          string `json:"time"`
	Type          string `json:"type"`
	Level         string `json:"level"`
	RunID         string `json:"run_id,omitempty"`
	Label         string `json:"label,omitempty"`
	Message       string `json:"message"`
}

type colorMode string

const (
	colorAuto    colorMode = "auto"
	colorAlways  colorMode = "always"
	colorNever   colorMode = "never"
	defaultColor           = colorAlways
)

func validColor(mode colorMode) bool {
	return mode == colorAuto || mode == colorAlways || mode == colorNever
}

func colorEnabled(out io.Writer, mode colorMode) bool {
	return mode == colorAlways || mode == colorAuto && activityTerminal(out)
}

type terminalOutput struct {
	mu     sync.Mutex
	out    io.Writer
	color  bool
	format outputFormat
	runID  string
	err    error
}

func newOutput(out io.Writer, mode colorMode) *terminalOutput {
	return &terminalOutput{out: out, color: colorEnabled(out, mode), format: outputHuman}
}

func newStreamOutput(out io.Writer, mode colorMode, format outputFormat, runID string) *terminalOutput {
	o := newOutput(out, mode)
	o.format, o.runID = format, runID
	return o
}

func (o *terminalOutput) printf(style activityStyle, format string, args ...any) {
	o.emit(time.Now(), "controller", "", fmt.Sprintf(format, args...), style)
}

// Each event is written immediately under the same lock, without a buffering queue.
func (o *terminalOutput) emit(now time.Time, kind, label, text string, style activityStyle) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return
	}
	if o.format == outputJSONL {
		event := logEvent{SchemaVersion: 1, Time: now.UTC().Format(time.RFC3339Nano), Type: kind, Level: logLevel(style), RunID: o.runID, Label: label, Message: strings.TrimSuffix(text, "\n")}
		o.err = json.NewEncoder(o.out).Encode(event)
		return
	}
	if label != "" {
		text = fmt.Sprintf("[%s] %s · %s\n", now.Format("15:04:05"), label, text)
	}
	if o.color {
		ending := ""
		if strings.HasSuffix(text, "\n") {
			ending = "\n"
		}
		text = string(style) + strings.TrimSuffix(text, "\n") + "\x1b[0m" + ending
	}
	_, o.err = fmt.Fprint(o.out, text)
}

func logLevel(style activityStyle) string {
	switch style {
	case activityFailure:
		return "error"
	case activityWarning:
		return "warning"
	case activityQuiet:
		return "debug"
	default:
		return "info"
	}
}

func (o *terminalOutput) error() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *terminalOutput) setColor(mode colorMode) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.color = colorEnabled(o.out, mode)
}

func (a *app) printf(style activityStyle, format string, args ...any) {
	out := a.output
	if out == nil {
		out = newOutput(os.Stdout, colorNever)
	}
	out.printf(style, format, args...)
}

func statusStyle(status string) activityStyle {
	switch status {
	case "running", "starting", "started", "already_running":
		return activityRunning
	case "complete", "registered", "ready", "ok":
		return activitySuccess
	case "interrupted", "selection_required":
		return activityWarning
	default:
		return activityFailure
	}
}
