package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// A read-only daemon query catches turns that outlive their codex exec client.
func daemonWorkerIdle(session string) error {
	env, err := executionEnvironment()
	if err != nil {
		return err
	}
	home := environmentValue(env, "CODEX_HOME")
	if home == "" {
		userHome := environmentValue(env, "HOME")
		home = filepath.Join(userHome, ".codex")
	}
	socket := filepath.Join(home, "app-server-control", "app-server-control.sock")
	if _, err := os.Stat(socket); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("cannot inspect daemon worker: %w", err)
	}
	c, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return fmt.Errorf("daemon worker state is unknown: %w", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(c, 4096)
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if _, err := fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", encoded); err != nil {
		return err
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return fmt.Errorf("daemon handshake failed: %w", err)
	}
	accept := sha1.Sum([]byte(encoded + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if response.StatusCode != http.StatusSwitchingProtocols || response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(accept[:]) {
		return errors.New("daemon worker state is unknown: unexpected WebSocket handshake")
	}
	if err := writeFrame(c, 1, []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"looper","version":"`+version+`"},"capabilities":{"experimentalApi":true}}}`)); err != nil {
		return err
	}
	if _, err := daemonReply(reader, c, 1); err != nil {
		return err
	}
	if err := writeFrame(c, 1, []byte(`{"method":"initialized"}`)); err != nil {
		return err
	}
	request, err := json.Marshal(map[string]any{"id": 2, "method": "thread/read", "params": map[string]any{"threadId": session, "includeTurns": false}})
	if err != nil {
		return err
	}
	if err := writeFrame(c, 1, request); err != nil {
		return err
	}
	result, err := daemonReply(reader, c, 2)
	if err != nil {
		return fmt.Errorf("daemon worker state is unknown: %w", err)
	}
	var data struct {
		Thread struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &data); err != nil {
		return err
	}
	switch data.Thread.Status.Type {
	case "idle", "notLoaded", "systemError":
		return nil
	case "active":
		return errors.New("the worker conversation still has an active daemon turn; observe it instead of resuming twice")
	default:
		return fmt.Errorf("unknown daemon worker status %q", data.Thread.Status.Type)
	}
}

func writeFrame(w io.Writer, opcode byte, body []byte) error {
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header := []byte{0x80 | opcode, 0x80}
	if len(body) < 126 {
		header[1] |= byte(len(body))
	} else if len(body) <= 65535 {
		header[1] |= 126
		header = binary.BigEndian.AppendUint16(header, uint16(len(body)))
	} else {
		header[1] |= 127
		header = binary.BigEndian.AppendUint64(header, uint64(len(body)))
	}
	frame := append(header, mask...)
	for i, b := range body {
		frame = append(frame, b^mask[i%4])
	}
	_, err := w.Write(frame)
	return err
}

func daemonReply(r io.Reader, w io.Writer, id int) (json.RawMessage, error) {
	var message []byte
	for range 128 {
		h := make([]byte, 2)
		if _, err := io.ReadFull(r, h); err != nil {
			return nil, err
		}
		n := uint64(h[1] & 127)
		if n == 126 {
			b := make([]byte, 2)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b))
		} else if n == 127 {
			b := make([]byte, 8)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b)
		}
		if h[1]&0x80 != 0 || n > 2<<20 || uint64(len(message))+n > 2<<20 {
			return nil, errors.New("invalid or oversized daemon response")
		}
		body := make([]byte, int(n))
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		switch h[0] & 15 {
		case 9:
			if len(body) > 125 {
				return nil, errors.New("oversized daemon ping")
			}
			if err := writeFrame(w, 10, body); err != nil {
				return nil, err
			}
			continue
		case 10:
			continue
		case 0, 1:
			message = append(message, body...)
		default:
			return nil, errors.New("daemon connection closed or sent an unsupported frame")
		}
		if h[0]&0x80 == 0 {
			continue
		}
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(message, &response); err != nil {
			return nil, err
		}
		message = nil
		if response.ID == id {
			if len(response.Error) > 0 && string(response.Error) != "null" {
				return nil, errors.New("daemon refused the read-only thread query")
			}
			return response.Result, nil
		}
	}
	return nil, errors.New("daemon notification limit exceeded")
}
