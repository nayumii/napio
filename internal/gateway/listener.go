package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// readBufferSize is large enough for any plausible Factorio datagram. napio
// only ever looks at the length and the source address of what it reads.
const readBufferSize = 2048

// Listener holds the public UDP endpoint while a server is asleep.
//
// The socket-ownership invariant from docs/ARCHITECTURE.md §2 is enforced
// here: only the supervisor calls Bind and Release, and it does so only on
// state transitions. Holding the socket (rather than sniffing traffic) means
// the kernel does not answer with ICMP port-unreachable, so the player's
// client stays in its normal retry loop while the server starts.
type Listener struct {
	addr   string
	filter *WakeFilter
	log    *slog.Logger

	mu      sync.Mutex
	conn    *net.UDPConn
	stopped chan struct{}
	wake    chan struct{}
}

// NewListener creates an unbound listener for addr ("0.0.0.0:34197").
func NewListener(addr string, filter *WakeFilter, log *slog.Logger) *Listener {
	return &Listener{
		addr:   addr,
		filter: filter,
		log:    log,
		wake:   make(chan struct{}, 1),
	}
}

// Wake fires (at most once per trigger) when traffic passes the filter.
func (l *Listener) Wake() <-chan struct{} { return l.wake }

// Bound reports whether napio currently holds the port.
func (l *Listener) Bound() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

// Bind takes the UDP port and starts listening. It is idempotent.
func (l *Listener) Bind() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		return nil
	}

	udpAddr, err := net.ResolveUDPAddr("udp", l.addr)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", l.addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("binding %s: %w", l.addr, err)
	}

	l.conn = conn
	l.stopped = make(chan struct{})
	l.filter.Reset()
	go l.read(conn, l.stopped)

	l.log.Debug("listening for wake-up traffic", "address", l.addr)
	return nil
}

// Release closes the socket so the Factorio server can bind the port itself.
// It blocks until the read loop has finished, so that when it returns the port
// really is free — Docker refuses to start a container whose published port is
// still held.
func (l *Listener) Release() {
	l.mu.Lock()
	conn, stopped := l.conn, l.stopped
	l.conn, l.stopped = nil, nil
	l.mu.Unlock()

	if conn == nil {
		return
	}
	_ = conn.Close()
	<-stopped
	l.log.Debug("released wake-up port", "address", l.addr)
}

func (l *Listener) read(conn *net.UDPConn, stopped chan struct{}) {
	defer close(stopped)
	buf := make([]byte, readBufferSize)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				l.log.Warn("wake-up listener read failed", "error", err)
			}
			return
		}
		if from == nil {
			continue
		}
		if l.filter.Observe(from.IP, n, time.Now()) {
			l.log.Info("connection attempt detected", "from", from.IP.String(), "address", l.addr)
			select {
			case l.wake <- struct{}{}:
			default: // a wake-up is already pending
			}
		}
	}
}

// WaitUntilFree blocks until the UDP port can be bound, which is how the
// supervisor confirms that a stopped server has really let go of it.
func WaitUntilFree(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		udpAddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return err
		}
		conn, err := net.ListenUDP("udp", udpAddr)
		if err == nil {
			conn.Close()
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return fmt.Errorf("port %s is still in use after %s: %w", addr, timeout, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// DisabledListener satisfies the listener contract without ever binding a
// port, for servers configured with wake.disabled (API-only control).
type DisabledListener struct{ never chan struct{} }

// NewDisabledListener returns a listener that never wakes anything.
func NewDisabledListener() *DisabledListener {
	return &DisabledListener{never: make(chan struct{})}
}

func (d *DisabledListener) Bind() error           { return nil }
func (d *DisabledListener) Release()              {}
func (d *DisabledListener) Bound() bool           { return false }
func (d *DisabledListener) Wake() <-chan struct{} { return d.never }
