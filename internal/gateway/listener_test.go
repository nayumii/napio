package gateway

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestListenerWakesOnRetriedTraffic(t *testing.T) {
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	filter := NewWakeFilter(4, 2, 5*time.Second, time.Second, nil)
	l := NewListener(addr, filter, testLogger())

	if err := l.Bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer l.Release()
	if !l.Bound() {
		t.Fatal("listener should report itself bound")
	}

	client, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	for i := 0; i < 2; i++ {
		if _, err := client.Write([]byte("connection request")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	select {
	case <-l.Wake():
	case <-time.After(2 * time.Second):
		t.Fatal("expected a wake-up from retried traffic")
	}
}

func TestReleaseFreesThePortForFactorio(t *testing.T) {
	// This is the port handover: Docker refuses to start a container whose
	// published port napio still holds, so Release must not return until the
	// port is genuinely free.
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	l := NewListener(addr, NewWakeFilter(4, 2, time.Second, 0, nil), testLogger())

	if err := l.Bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}
	l.Release()
	if l.Bound() {
		t.Fatal("listener should report itself unbound after Release")
	}

	udpAddr, _ := net.ResolveUDPAddr("udp", addr)
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatalf("the port must be bindable immediately after Release: %v", err)
	}
	conn.Close()

	// Rebinding, as happens after every shutdown, must work.
	if err := l.Bind(); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	l.Release()
}

func TestBindIsIdempotentAndReleaseIsSafeWhenUnbound(t *testing.T) {
	port := freePort(t)
	l := NewListener(fmt.Sprintf("127.0.0.1:%d", port), NewWakeFilter(4, 2, time.Second, 0, nil), testLogger())

	l.Release() // must not panic when never bound
	if err := l.Bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := l.Bind(); err != nil {
		t.Fatalf("second bind should be a no-op, got %v", err)
	}
	l.Release()
	l.Release()
}

func TestBindFailsWhenThePortIsTaken(t *testing.T) {
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	udpAddr, _ := net.ResolveUDPAddr("udp", addr)
	held, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatalf("holding the port: %v", err)
	}
	defer held.Close()

	l := NewListener(addr, NewWakeFilter(4, 2, time.Second, 0, nil), testLogger())
	if err := l.Bind(); err == nil {
		l.Release()
		t.Fatal("expected bind to fail while the port is held")
	}
}

func TestWaitUntilFree(t *testing.T) {
	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if err := WaitUntilFree(addr, time.Second); err != nil {
		t.Fatalf("a free port should be reported free: %v", err)
	}

	udpAddr, _ := net.ResolveUDPAddr("udp", addr)
	held, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		t.Fatalf("holding the port: %v", err)
	}
	defer held.Close()
	if err := WaitUntilFree(addr, 300*time.Millisecond); err == nil {
		t.Fatal("a held port must not be reported free")
	}
}
