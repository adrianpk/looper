package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// Explicit color wins over inherited suppression; auto requires an actual capable terminal.
func TestColorMode(t *testing.T) {
	tty, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	for _, tc := range []struct {
		name          string
		mode          colorMode
		noColor, term string
		out           io.Writer
		want          bool
	}{
		{"terminal", colorAuto, "", "xterm-256color", tty, true},
		{"inherited suppression", colorAuto, "1", "xterm-256color", tty, false},
		{"explicit override", colorAlways, "1", "dumb", tty, true},
		{"disabled", colorNever, "", "xterm-256color", tty, false},
		{"redirected", colorAuto, "", "xterm-256color", io.Discard, false},
		{"forced pipe", colorAlways, "1", "dumb", io.Discard, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tc.noColor)
			t.Setenv("TERM", tc.term)
			if got := colorEnabled(tc.out, tc.mode); got != tc.want {
				t.Fatalf("color=%v, want %v", got, tc.want)
			}
		})
	}
}

// Runtime color changes apply to worker events and controller phases without touching saved progress.
func TestRuntimeColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer
	config := t.TempDir() + "/config.json"
	m, stop, err := startActivity(config, true, newOutput(&out, colorAuto))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	a := &app{output: m.output}
	for _, tc := range []struct {
		mode colorMode
		want bool
	}{{colorAlways, true}, {colorNever, false}, {colorAuto, false}} {
		if got, err := activityRequest(config, "color:"+string(tc.mode)); err != nil || got != "Color: "+string(tc.mode) {
			t.Fatalf("color control: %q %v", got, err)
		}
		out.Reset()
		m.begin("Test worker", "worker.started")
		a.printf(activitySuccess, "Controller phase\n")
		m.end()
		if strings.Contains(out.String(), "\x1b[") != tc.want {
			t.Fatalf("runtime color %s: %q", tc.mode, out.String())
		}
	}
}
