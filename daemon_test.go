package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readClientFrame(r io.Reader) ([]byte, byte, error) {
	h := make([]byte, 2)
	if _, err := io.ReadFull(r, h); err != nil {
		return nil, 0, err
	}
	if h[1]&0x80 == 0 {
		return nil, 0, fmt.Errorf("client frame is not masked")
	}
	n := int(h[1] & 127)
	if n == 126 {
		b := make([]byte, 2)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, 0, err
		}
		n = int(binary.BigEndian.Uint16(b))
	}
	if n > 4096 {
		return nil, 0, fmt.Errorf("unexpected client frame length")
	}
	mask := make([]byte, 4)
	if _, err := io.ReadFull(r, mask); err != nil {
		return nil, 0, err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, 0, err
	}
	for i := range b {
		b[i] ^= mask[i%4]
	}
	return b, h[0] & 15, nil
}

func serverFrame(w io.Writer, opcode byte, final bool, body []byte) error {
	h := []byte{opcode, byte(len(body))}
	if final {
		h[0] |= 128
	}
	if len(body) >= 126 {
		h[1] = 126
		h = binary.BigEndian.AppendUint16(h, uint16(len(body)))
	}
	_, err := w.Write(append(h, body...))
	return err
}

// Querying the actual transport prevents an orphaned daemon turn from being resumed twice.
func TestDaemonOwnership(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		fail   bool
	}{
		{"idle", "idle", false},
		{"unloaded", "notLoaded", false},
		{"active orphan", "active", true},
		{"unknown state", "futureState", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			dir := filepath.Join(home, "app-server-control")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(dir, "app-server-control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			done := make(chan error, 1)
			go func() {
				done <- serveOwnershipQuery(listener, tc.status)
			}()
			err = daemonWorkerIdle("expected-conversation")
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected ownership decision: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func serveOwnershipQuery(listener net.Listener, status string) error {
	c, err := listener.Accept()
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return err
	}
	r := bufio.NewReader(c)
	request, err := http.ReadRequest(r)
	if err != nil {
		return err
	}
	accept := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if _, err := fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:])); err != nil {
		return err
	}
	init, _, err := readClientFrame(r)
	if err != nil || !strings.Contains(string(init), `"method":"initialize"`) {
		return fmt.Errorf("invalid initialization: %v", err)
	}
	// Exercise a ping and fragmented JSON, not just the simplest single-frame response.
	if err := serverFrame(c, 9, true, []byte("ping")); err != nil {
		return err
	}
	pong, opcode, err := readClientFrame(r)
	if err != nil || opcode != 10 || string(pong) != "ping" {
		return fmt.Errorf("invalid pong: %v", err)
	}
	if err := serverFrame(c, 1, false, []byte(`{"id":1,`)); err != nil {
		return err
	}
	if err := serverFrame(c, 0, true, []byte(`"result":{}}`)); err != nil {
		return err
	}
	initialized, _, err := readClientFrame(r)
	if err != nil || !strings.Contains(string(initialized), `"method":"initialized"`) {
		return fmt.Errorf("invalid initialized notification: %v", err)
	}
	query, _, err := readClientFrame(r)
	if err != nil {
		return err
	}
	var msg struct {
		Method string `json:"method"`
		Params struct {
			ThreadID     string `json:"threadId"`
			IncludeTurns bool   `json:"includeTurns"`
		} `json:"params"`
	}
	if err := json.Unmarshal(query, &msg); err != nil || msg.Method != "thread/read" || msg.Params.ThreadID != "expected-conversation" || msg.Params.IncludeTurns {
		return fmt.Errorf("query changed authority or conversation: %v", err)
	}
	body := []byte(fmt.Sprintf(`{"id":2,"result":{"thread":{"status":{"type":%q}}}}`, status))
	return serverFrame(c, 1, true, body)
}
