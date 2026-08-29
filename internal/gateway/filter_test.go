package gateway

import (
	"net"
	"testing"
	"time"
)

func TestOneDatagramDoesNotWakeTheServer(t *testing.T) {
	// This is the port-scanner case: a single probe must not start a server.
	f := NewWakeFilter(4, 2, 5*time.Second, 30*time.Second, nil)
	now := time.Now()
	if f.Observe(net.ParseIP("203.0.113.10"), 64, now) {
		t.Fatal("a single datagram must not trigger a wake-up")
	}
}

func TestRetryingClientWakesTheServer(t *testing.T) {
	// This is the player case: the Factorio client retries its connection
	// request several times within its ~10s connect window.
	f := NewWakeFilter(4, 2, 5*time.Second, 30*time.Second, nil)
	now := time.Now()
	f.Observe(net.ParseIP("203.0.113.10"), 64, now)
	if !f.Observe(net.ParseIP("203.0.113.10"), 64, now.Add(300*time.Millisecond)) {
		t.Fatal("a retrying client must trigger a wake-up")
	}
}

func TestPacketsOutsideTheWindowDoNotAccumulate(t *testing.T) {
	f := NewWakeFilter(4, 2, 5*time.Second, 30*time.Second, nil)
	now := time.Now()
	f.Observe(net.ParseIP("203.0.113.10"), 64, now)
	if f.Observe(net.ParseIP("203.0.113.10"), 64, now.Add(time.Minute)) {
		t.Fatal("datagrams a minute apart are not one connection attempt")
	}
}

func TestDifferentSourcesDoNotAccumulate(t *testing.T) {
	f := NewWakeFilter(4, 2, 5*time.Second, 30*time.Second, nil)
	now := time.Now()
	f.Observe(net.ParseIP("203.0.113.10"), 64, now)
	if f.Observe(net.ParseIP("203.0.113.11"), 64, now) {
		t.Fatal("two different scanners are not one player")
	}
}

func TestTinyDatagramsAreIgnored(t *testing.T) {
	f := NewWakeFilter(8, 1, 5*time.Second, 30*time.Second, nil)
	if f.Observe(net.ParseIP("203.0.113.10"), 1, time.Now()) {
		t.Fatal("a datagram below min_packet_size must be ignored")
	}
}

func TestCooldownSuppressesRepeatedWakeUps(t *testing.T) {
	f := NewWakeFilter(4, 1, 5*time.Second, 30*time.Second, nil)
	now := time.Now()
	if !f.Observe(net.ParseIP("203.0.113.10"), 64, now) {
		t.Fatal("the first datagram should trigger")
	}
	if f.Observe(net.ParseIP("203.0.113.10"), 64, now.Add(time.Second)) {
		t.Fatal("the cooldown must suppress a second trigger")
	}
	if !f.Observe(net.ParseIP("203.0.113.10"), 64, now.Add(31*time.Second)) {
		t.Fatal("the trigger should work again once the cooldown has elapsed")
	}
}

func TestAllowFromRestrictsWhoCanWakeTheServer(t *testing.T) {
	nets, err := ParseCIDRs([]string{"192.168.0.0/16"})
	if err != nil {
		t.Fatalf("parsing CIDRs: %v", err)
	}
	f := NewWakeFilter(4, 1, 5*time.Second, 0, nets)
	now := time.Now()
	if f.Observe(net.ParseIP("203.0.113.10"), 64, now) {
		t.Fatal("an address outside allow_from must not wake the server")
	}
	if !f.Observe(net.ParseIP("192.168.1.5"), 64, now) {
		t.Fatal("an address inside allow_from must wake the server")
	}
}

func TestFilterMemoryIsBounded(t *testing.T) {
	// A spoofed-source flood must not grow napio's memory without bound.
	f := NewWakeFilter(4, 5, time.Second, 0, nil)
	now := time.Now()
	for i := 0; i < maxTrackedSources*2; i++ {
		ip := net.IPv4(10, byte(i>>16), byte(i>>8), byte(i))
		f.Observe(ip, 64, now)
	}
	f.mu.Lock()
	tracked := len(f.recent)
	f.mu.Unlock()
	if tracked > maxTrackedSources+1 {
		t.Fatalf("filter tracked %d sources, which is unbounded growth", tracked)
	}
}

func TestResetForgetsHistoryAndCooldown(t *testing.T) {
	f := NewWakeFilter(4, 1, 5*time.Second, time.Hour, nil)
	now := time.Now()
	f.Observe(net.ParseIP("203.0.113.10"), 64, now)
	f.Reset()
	if !f.Observe(net.ParseIP("203.0.113.10"), 64, now.Add(time.Second)) {
		t.Fatal("Reset must clear the cooldown so the next player is judged fresh")
	}
}
