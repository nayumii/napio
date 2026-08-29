// Package rcon implements the Source RCON protocol as spoken by the Factorio
// headless server.
//
// napio uses RCON for three things: readiness detection (the Factorio server
// only starts its RCON interface once the save is loaded), player counting,
// and graceful shutdown via /save followed by /quit.
package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Packet types from the Source RCON specification. Note that
// SERVERDATA_EXECCOMMAND and SERVERDATA_AUTH_RESPONSE share the value 2; which
// one a packet is depends on its direction.
const (
	typeResponse     = 0 // SERVERDATA_RESPONSE_VALUE
	typeCommand      = 2 // SERVERDATA_EXECCOMMAND (client -> server)
	typeAuthResponse = 2 // SERVERDATA_AUTH_RESPONSE (server -> client)
	typeAuth         = 3 // SERVERDATA_AUTH
)

const (
	maxPacketSize = 4096
	minPacketSize = 10
)

// ErrAuthFailed is returned when the server rejects the password.
var ErrAuthFailed = errors.New("rcon: authentication failed")

// Conn is an authenticated RCON connection. It is not safe for concurrent use.
type Conn struct {
	conn    net.Conn
	timeout time.Duration
	nextID  int32
}

// Dial connects and authenticates. The returned Conn must be closed.
func Dial(ctx context.Context, address, password string, timeout time.Duration) (*Conn, error) {
	d := net.Dialer{Timeout: timeout}
	nc, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("rcon: dial %s: %w", address, err)
	}
	c := &Conn{conn: nc, timeout: timeout, nextID: 1}
	if err := c.auth(password); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) auth(password string) error {
	id := c.id()
	if err := c.write(id, typeAuth, password); err != nil {
		return err
	}
	// Factorio replies with a single AUTH_RESPONSE packet: the request id on
	// success, -1 on failure (verified against a live 2.0.77 server). Valve's
	// implementation prefixes an empty RESPONSE_VALUE, so that is skipped for
	// compatibility with other servers.
	for {
		resp, err := c.read()
		if err != nil {
			return err
		}
		if resp.id == -1 {
			return ErrAuthFailed
		}
		if resp.typ == typeResponse {
			continue
		}
		if resp.id != id {
			return fmt.Errorf("rcon: unexpected auth response id %d", resp.id)
		}
		return nil
	}
}

// Exec runs a console command and returns the server's response body.
func (c *Conn) Exec(command string) (string, error) {
	id := c.id()
	if err := c.write(id, typeCommand, command); err != nil {
		return "", err
	}
	resp, err := c.read()
	if err != nil {
		return "", err
	}
	if resp.id != id {
		return "", fmt.Errorf("rcon: response id %d does not match request %d", resp.id, id)
	}
	return resp.body, nil
}

// ExecNoReply sends a command without waiting for a reply. It exists for
// /quit, which terminates the server before it can answer.
func (c *Conn) ExecNoReply(command string) error {
	return c.write(c.id(), typeCommand, command)
}

func (c *Conn) Close() error { return c.conn.Close() }

func (c *Conn) id() int32 {
	c.nextID++
	if c.nextID <= 0 { // never collide with the -1 auth-failure sentinel
		c.nextID = 1
	}
	return c.nextID
}

func (c *Conn) write(id, typ int32, body string) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}
	payload := make([]byte, 0, 10+len(body))
	payload = binary.LittleEndian.AppendUint32(payload, uint32(id))
	payload = binary.LittleEndian.AppendUint32(payload, uint32(typ))
	payload = append(payload, body...)
	payload = append(payload, 0, 0)

	frame := make([]byte, 0, 4+len(payload))
	frame = binary.LittleEndian.AppendUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)

	if _, err := c.conn.Write(frame); err != nil {
		return fmt.Errorf("rcon: write: %w", err)
	}
	return nil
}

type packet struct {
	id   int32
	typ  int32
	body string
}

func (c *Conn) read() (packet, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return packet{}, err
	}
	var header [4]byte
	if _, err := io.ReadFull(c.conn, header[:]); err != nil {
		return packet{}, fmt.Errorf("rcon: read length: %w", err)
	}
	size := int32(binary.LittleEndian.Uint32(header[:]))
	// The length is attacker-influenced only by a compromised server, but it
	// still sizes an allocation, so it is bounded.
	if size < minPacketSize || size > maxPacketSize {
		return packet{}, fmt.Errorf("rcon: implausible packet length %d", size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return packet{}, fmt.Errorf("rcon: read body: %w", err)
	}
	p := packet{
		id:  int32(binary.LittleEndian.Uint32(payload[0:4])),
		typ: int32(binary.LittleEndian.Uint32(payload[4:8])),
	}
	p.body = strings.TrimRight(string(payload[8:]), "\x00")
	return p, nil
}
