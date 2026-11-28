package congestion

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sagernet/quic-go/congestion"
)

const (
	// DefaultCarefulResumeLifetime is how long a set of saved congestion
	// control parameters may be reused after it was last observed, when the
	// configuration does not say. RFC 9959 asks for minutes on paths whose
	// conditions change; the paths this is used on are lossy and shared.
	DefaultCarefulResumeLifetime = 10 * time.Minute

	// carefulResumeLease is how long one connection holds a set of saved
	// parameters before another connection may take them. RFC 9959 lets only
	// one connection at a time use a set; the lease keeps a connection that
	// closed before it finished from blocking the set for good.
	carefulResumeLease = 30 * time.Second

	// carefulResumeFlushInterval is how often a changed store is written to
	// its state file.
	carefulResumeFlushInterval = 10 * time.Second

	carefulResumeStateVersion = 1
)

// CarefulResumeParams is the congestion control state that BBR observed on a
// path (RFC 9959, Section 3.1): the bottleneck bandwidth (BBR.max_bw), the
// minimum round trip time (saved_rtt) and the volume of data delivered in one
// round trip (saved_cwnd).
type CarefulResumeParams struct {
	Bandwidth Bandwidth
	MinRTT    time.Duration
	CWND      congestion.ByteCount
}

type carefulResumeEntry struct {
	params    CarefulResumeParams
	savedAt   time.Time
	claimedBy uint64
	claimedAt time.Time
}

// CarefulResumeStore keeps one set of saved congestion control parameters
// for each remote endpoint (RFC 9959, Section 3.1), so that a later connection
// to the same endpoint can resume from them instead of from slow start. It is
// safe for concurrent use by many connections.
//
// When the store has a state file, it is loaded when the store is made and
// written every few seconds after a change, so that the saved parameters
// survive a restart of the process.
type CarefulResumeStore struct {
	lifetime time.Duration
	path     string
	now      func() time.Time

	mu       sync.Mutex
	entries  map[string]*carefulResumeEntry
	nextID   uint64
	dirty    bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewCarefulResumeStore makes a store whose entries can be reused for
// lifetime after they were last observed (DefaultCarefulResumeLifetime if
// lifetime is zero). If path is not empty, the store loads the entries saved
// there and saves its entries there until Close; a missing or unreadable
// state file starts the store empty.
func NewCarefulResumeStore(lifetime time.Duration, path string) (*CarefulResumeStore, error) {
	if lifetime <= 0 {
		lifetime = DefaultCarefulResumeLifetime
	}
	s := &CarefulResumeStore{
		lifetime: lifetime,
		path:     path,
		now:      time.Now,
		entries:  make(map[string]*carefulResumeEntry),
	}
	if path == "" {
		return s, nil
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.flushLoop()
	return s, nil
}

// CarefulResumeKey returns the Remote Endpoint (RFC 9959, Section 2.2) of a
// connection: the peer's IP address. The port is left out, because a new
// connection from the same peer comes from a new port.
func CarefulResumeKey(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	if udp, ok := addr.(*net.UDPAddr); ok {
		if ip, ok := netip.AddrFromSlice(udp.IP); ok {
			return ip.Unmap().String()
		}
	}
	if ap, err := netip.ParseAddrPort(addr.String()); err == nil {
		return ap.Addr().Unmap().String()
	}
	return addr.String()
}

// claim gives the saved parameters for key to one connection. It fails when
// there are none, when they are older than the lifetime (they are deleted
// then), or when another connection holds them.
func (s *CarefulResumeStore) claim(key string) (CarefulResumeParams, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key]
	if e == nil {
		return CarefulResumeParams{}, 0, false
	}
	now := s.now()
	if now.Sub(e.savedAt) > s.lifetime {
		delete(s.entries, key)
		s.dirty = true
		return CarefulResumeParams{}, 0, false
	}
	if e.claimedBy != 0 && now.Sub(e.claimedAt) < carefulResumeLease {
		return CarefulResumeParams{}, 0, false
	}
	s.nextID++
	e.claimedBy = s.nextID
	e.claimedAt = now
	return e.params, e.claimedBy, true
}

// holds reports whether the claim id still holds parameters for key that
// have not outlived the lifetime.
func (s *CarefulResumeStore) holds(key string, id uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key]
	return e != nil && e.claimedBy == id && s.now().Sub(e.savedAt) <= s.lifetime
}

// release ends a claim, so that another connection may use the parameters.
func (s *CarefulResumeStore) release(key string, id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[key]; e != nil && e.claimedBy == id {
		e.claimedBy = 0
	}
}

// forget deletes the parameters for key (RFC 9959, Section 3.5: Safe Retreat
// removes the saved information).
func (s *CarefulResumeStore) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[key]; ok {
		delete(s.entries, key)
		s.dirty = true
	}
}

// observe saves the parameters a connection observed for key, replacing the
// ones saved before (RFC 9959, Section 3.1). An observation made while the
// sender was application limited replaces a saved set only when it is
// larger (RFC 9959, Section 4.1).
func (s *CarefulResumeStore) observe(key string, p CarefulResumeParams, appLimited bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key]
	now := s.now()
	if e != nil && appLimited && e.params.CWND >= p.CWND && now.Sub(e.savedAt) <= s.lifetime {
		return
	}
	if e == nil {
		e = &carefulResumeEntry{}
		s.entries[key] = e
	}
	e.params = p
	e.savedAt = now
	s.dirty = true
}

// Len returns the number of saved sets, whether or not they have expired.
func (s *CarefulResumeStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Flush deletes all saved parameters, for example after a change of the
// configuration (RFC 9959, Section 4.3.1).
func (s *CarefulResumeStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.entries)
	s.dirty = true
}

// Close stops the background saving and writes the state file a last time.
func (s *CarefulResumeStore) Close() error {
	if s.path == "" {
		return nil
	}
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
	return s.save()
}

func (s *CarefulResumeStore) flushLoop() {
	defer close(s.done)
	ticker := time.NewTicker(carefulResumeFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			_ = s.save()
		}
	}
}

type carefulResumeState struct {
	Version int                                `json:"version"`
	Entries map[string]carefulResumeStateEntry `json:"entries"`
}

type carefulResumeStateEntry struct {
	BandwidthBPS uint64    `json:"bandwidth_bps"`
	MinRTTMicros int64     `json:"min_rtt_us"`
	CWND         uint64    `json:"cwnd_bytes"`
	SavedAt      time.Time `json:"saved_at"`
}

func (s *CarefulResumeStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state carefulResumeState
	if json.Unmarshal(data, &state) != nil || state.Version != carefulResumeStateVersion {
		// A damaged or older file only costs the saved parameters.
		return nil
	}
	now := s.now()
	for key, se := range state.Entries {
		if now.Sub(se.SavedAt) > s.lifetime || se.BandwidthBPS == 0 || se.MinRTTMicros <= 0 {
			continue
		}
		s.entries[key] = &carefulResumeEntry{
			params: CarefulResumeParams{
				Bandwidth: Bandwidth(se.BandwidthBPS),
				MinRTT:    time.Duration(se.MinRTTMicros) * time.Microsecond,
				CWND:      congestion.ByteCount(se.CWND),
			},
			savedAt: se.SavedAt,
		}
	}
	return nil
}

func (s *CarefulResumeStore) save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	state := carefulResumeState{
		Version: carefulResumeStateVersion,
		Entries: make(map[string]carefulResumeStateEntry, len(s.entries)),
	}
	now := s.now()
	for key, e := range s.entries {
		if now.Sub(e.savedAt) > s.lifetime {
			continue
		}
		state.Entries[key] = carefulResumeStateEntry{
			BandwidthBPS: uint64(e.params.Bandwidth),
			MinRTTMicros: e.params.MinRTT.Microseconds(),
			CWND:         uint64(e.params.CWND),
			SavedAt:      e.savedAt.UTC(),
		}
	}
	s.dirty = false
	s.mu.Unlock()

	data, err := json.Marshal(state)
	if err == nil {
		err = writeFileAtomic(s.path, data)
	}
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return err
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
