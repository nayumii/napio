// Package gateway owns the UDP port that players connect to while a Factorio
// server is asleep.
//
// It never parses Factorio packets. The protocol is undocumented and changes
// between game versions; anything built on packet layouts would rot, and its
// failure mode — misreading a real player as noise and never waking the
// server — is the worst one available. Instead napio treats an incoming
// datagram as a doorbell and uses a behavioural filter to tell a player from a
// port scanner: a real client retries its connection request several times
// within a few seconds, a scanner sends one packet and moves on.
package gateway

import (
	"net"
	"sync"
	"time"
)

// maxTrackedSources bounds the filter's memory against a spoofed-source flood.
const maxTrackedSources = 4096

// WakeFilter decides whether observed traffic should wake a server.
type WakeFilter struct {
	MinPacketSize int
	Packets       int
	Window        time.Duration
	Cooldown      time.Duration
	AllowFrom     []*net.IPNet

	mu          sync.Mutex
	recent      map[string][]time.Time
	lastTrigger time.Time
}

// NewWakeFilter builds a filter. packets < 1 is treated as 1.
func NewWakeFilter(minPacketSize, packets int, window, cooldown time.Duration, allowFrom []*net.IPNet) *WakeFilter {
	if packets < 1 {
		packets = 1
	}
	return &WakeFilter{
		MinPacketSize: minPacketSize,
		Packets:       packets,
		Window:        window,
		Cooldown:      cooldown,
		AllowFrom:     allowFrom,
		recent:        make(map[string][]time.Time),
	}
}

// Observe records a datagram and reports whether it should trigger a wake-up.
func (f *WakeFilter) Observe(from net.IP, size int, now time.Time) bool {
	if size < f.MinPacketSize {
		return false
	}
	if !f.allowed(from) {
		return false
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.lastTrigger.IsZero() && now.Sub(f.lastTrigger) < f.Cooldown {
		return false
	}

	key := from.String()
	hits := append(prune(f.recent[key], now, f.Window), now)
	f.recent[key] = hits

	if len(f.recent) > maxTrackedSources {
		f.evict(now)
	}

	if len(hits) >= f.Packets {
		f.lastTrigger = now
		delete(f.recent, key)
		return true
	}
	return false
}

// Reset forgets everything observed so far, including the cooldown. It is
// called when a server returns to sleep so the next player is not judged by
// traffic from the previous session.
func (f *WakeFilter) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recent = make(map[string][]time.Time)
	f.lastTrigger = time.Time{}
}

func (f *WakeFilter) allowed(ip net.IP) bool {
	if len(f.AllowFrom) == 0 {
		return true
	}
	for _, n := range f.AllowFrom {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// evict drops sources with no observation inside the window; if that is not
// enough (a flood of distinct spoofed sources), it clears the table outright.
// Losing history costs at most one extra retry from a real player.
func (f *WakeFilter) evict(now time.Time) {
	for key, hits := range f.recent {
		if kept := prune(hits, now, f.Window); len(kept) == 0 {
			delete(f.recent, key)
		} else {
			f.recent[key] = kept
		}
	}
	if len(f.recent) > maxTrackedSources {
		f.recent = make(map[string][]time.Time)
	}
}

func prune(hits []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	i := 0
	for ; i < len(hits); i++ {
		if hits[i].After(cutoff) {
			break
		}
	}
	return hits[i:]
}

// ParseCIDRs converts configuration strings into networks.
func ParseCIDRs(entries []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, entry := range entries {
		_, n, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
