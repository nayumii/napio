package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeServer speaks just enough Source RCON to exercise the client.
type fakeServer struct {
	listener net.Listener
	password string
	replies  map[string]string
}

func startFakeServer(t *testing.T, password string, replies map[string]string) *fakeServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeServer{listener: l, password: password, replies: replies}
	go s.serve()
	t.Cleanup(func() { l.Close() })
	return s
}

func (s *fakeServer) addr() string { return s.listener.Addr().String() }

func (s *fakeServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeServer) handle(conn net.Conn) {
	defer conn.Close()
	for {
		id, typ, body, err := readPacket(conn)
		if err != nil {
			return
		}
		switch typ {
		case typeAuth:
			// Factorio answers with a single AUTH_RESPONSE packet, carrying
			// the request id on success and -1 on failure. Captured from a
			// live 2.0.77 server.
			if body != s.password {
				_ = writePacket(conn, -1, typeAuthResponse, "")
				return
			}
			_ = writePacket(conn, id, typeAuthResponse, "")
		case typeCommand:
			reply, ok := s.replies[body]
			if !ok {
				reply = "Unknown command"
			}
			_ = writePacket(conn, id, typeResponse, reply)
		}
	}
}

func readPacket(conn net.Conn) (int32, int32, string, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return 0, 0, "", err
	}
	size := int32(binary.LittleEndian.Uint32(header[:]))
	if size < 10 || size > 4096 {
		return 0, 0, "", errors.New("bad size")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, 0, "", err
	}
	id := int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ := int32(binary.LittleEndian.Uint32(payload[4:8]))
	return id, typ, strings.TrimRight(string(payload[8:]), "\x00"), nil
}

func writePacket(conn net.Conn, id, typ int32, body string) error {
	payload := make([]byte, 0, 10+len(body))
	payload = binary.LittleEndian.AppendUint32(payload, uint32(id))
	payload = binary.LittleEndian.AppendUint32(payload, uint32(typ))
	payload = append(payload, body...)
	payload = append(payload, 0, 0)
	frame := binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))
	_, err := conn.Write(append(frame, payload...))
	return err
}

func TestExecRoundTrip(t *testing.T) {
	s := startFakeServer(t, "secret", map[string]string{
		"/players online": "Online players (2):\n  Alice (online)\n  Bob (online)\n",
		"/version":        "2.0.77\n",
	})

	conn, err := Dial(context.Background(), s.addr(), "secret", 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	reply, err := conn.Exec("/players online")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.HasPrefix(reply, "Online players (2)") {
		t.Fatalf("unexpected reply %q", reply)
	}

	// A second command on the same connection must still match ids correctly.
	if reply, err = conn.Exec("/version"); err != nil {
		t.Fatalf("second exec: %v", err)
	}
	if strings.TrimSpace(reply) != "2.0.77" {
		t.Fatalf("unexpected version reply %q", reply)
	}
}

func TestWrongPasswordIsRejected(t *testing.T) {
	s := startFakeServer(t, "secret", nil)
	_, err := Dial(context.Background(), s.addr(), "wrong", 2*time.Second)
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got %v", err)
	}
}

func TestDialFailsWhenNothingIsListening(t *testing.T) {
	// This is the normal case while a Factorio server is still loading, and
	// it is exactly how napio detects that it is not ready yet.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	if _, err := Dial(context.Background(), addr, "secret", 500*time.Millisecond); err == nil {
		t.Fatal("expected a dial error")
	}
}

func TestExecNoReplyDoesNotBlock(t *testing.T) {
	// /quit terminates the server before it can answer.
	s := startFakeServer(t, "secret", nil)
	conn, err := Dial(context.Background(), s.addr(), "secret", 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.ExecNoReply("/quit"); err != nil {
		t.Fatalf("exec no reply: %v", err)
	}
}
