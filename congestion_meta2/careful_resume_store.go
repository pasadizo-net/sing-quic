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

	// carefulResumeMaxEntries bounds the store. When it is full, a new peer
	// takes the place of the set observed longest ago.
	carefulResumeMaxEntries = 4096

	// carefulResumeSweepInterval is how often the background loop deletes
	// expired sets and, with a state file, writes a changed store to it.
	carefulResumeSweepInterval = 10 * time.Second

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
// safe for concurrent use by many connections. It holds at most
// carefulResumeMaxEntries sets.
//
// Start runs a background loop that deletes expired sets and, when the store
// has a state file, writes the store there after a change, so that the saved
// parameters survive a restart of the process. Close stops the loop.
type CarefulResumeStore struct {
	lifetime time.Duration
	path     string
	now      func() time.Time
	log      func(format string, a ...any)

	mu      sync.Mutex
	entries map[string]*carefulResumeEntry
	nextID  uint64
	dirty   bool

	loopMu  sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	lastErr string
}

// NewCarefulResumeStore makes a store whose entries can be reused for
// lifetime after they were last observed (DefaultCarefulResumeLifetime if
// lifetime is zero). If path is not empty, the store loads the entries saved
// there; a state file that is missing, cannot be read or is damaged starts
// the store empty, with a warning through log for the last two, so that it
// never stops a server from starting. log may be nil.
func NewCarefulResumeStore(lifetime time.Duration, path string, log func(format string, a ...any)) *CarefulResumeStore {
	if lifetime <= 0 {
		lifetime = DefaultCarefulResumeLifetime
	}
	s := &CarefulResumeStore{
		lifetime: lifetime,
		path:     path,
		now:      time.Now,
		log:      log,
		entries:  make(map[string]*carefulResumeEntry),
	}
	if path != "" {
		if err := s.load(); err != nil {
			s.logf("careful resume: state file %s not loaded, starting empty: %v", path, err)
		}
	}
	return s
}

// CarefulResumeKey returns the Remote Endpoint (RFC 9959, Section 2.2) of a
// connection: the peer's IPv4 address, or the /64 of its IPv6 address, so
// that a host that changes its IPv6 address within its network keeps one
// entry. The port is left out, because a new connection from the same peer
// comes from a new port.
func CarefulResumeKey(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	var ip netip.Addr
	if udp, ok := addr.(*net.UDPAddr); ok {
		ip, _ = netip.AddrFromSlice(udp.IP)
	} else if ap, err := netip.ParseAddrPort(addr.String()); err == nil {
		ip = ap.Addr()
	}
	if !ip.IsValid() {
		return addr.String()
	}
	ip = ip.Unmap()
	if ip.Is6() {
		return netip.PrefixFrom(ip, 64).Masked().String()
	}
	return ip.String()
}

func (s *CarefulResumeStore) expired(e *carefulResumeEntry, now time.Time) bool {
	// A time in the future comes from a clock that was wrong when it was
	// saved, such as a Raspberry Pi that booted before NTP set its clock.
	return e.savedAt.After(now) || now.Sub(e.savedAt) > s.lifetime
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
	if s.expired(e, now) {
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
	return e != nil && e.claimedBy == id && !s.expired(e, s.now())
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
	if e != nil && appLimited && e.params.CWND >= p.CWND && !s.expired(e, now) {
		return
	}
	if e == nil {
		if len(s.entries) >= carefulResumeMaxEntries {
			s.sweepLocked(now)
		}
		if len(s.entries) >= carefulResumeMaxEntries {
			s.evictOldestLocked()
		}
		e = &carefulResumeEntry{}
		s.entries[key] = e
	}
	e.params = p
	e.savedAt = now
	s.dirty = true
}

// sweepLocked deletes the expired sets. s.mu must be held.
func (s *CarefulResumeStore) sweepLocked(now time.Time) {
	for key, e := range s.entries {
		if s.expired(e, now) {
			delete(s.entries, key)
			s.dirty = true
		}
	}
}

// evictOldestLocked deletes the set observed longest ago. s.mu must be held.
func (s *CarefulResumeStore) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for key, e := range s.entries {
		if oldestKey == "" || e.savedAt.Before(oldest) {
			oldestKey, oldest = key, e.savedAt
		}
	}
	if oldestKey != "" {
		delete(s.entries, oldestKey)
		s.dirty = true
	}
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

// Start starts the background loop. Calling it again does nothing.
func (s *CarefulResumeStore) Start() {
	s.loopMu.Lock()
	defer s.loopMu.Unlock()
	if s.stop != nil {
		return
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go s.loop(s.stop, s.done)
}

// Close stops the background loop and, when the store has a state file and
// has changed, writes the file a last time. A store that was never started
// writes nothing, so a configuration check does not touch the file.
func (s *CarefulResumeStore) Close() error {
	s.loopMu.Lock()
	stop, done := s.stop, s.done
	s.stop = nil
	s.loopMu.Unlock()
	if stop == nil {
		return nil
	}
	close(stop)
	<-done
	if s.path == "" {
		return nil
	}
	return s.save()
}

func (s *CarefulResumeStore) loop(stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(carefulResumeSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			s.sweepLocked(s.now())
			s.mu.Unlock()
			if s.path != "" {
				s.reportSaveError(s.save())
			}
		}
	}
}

// reportSaveError logs a failure to write the state file when its cause
// differs from the last one, and the recovery after a failure, so that a broken file
// does not log every few seconds.
func (s *CarefulResumeStore) reportSaveError(err error) {
	// The innermost error, without the name of the temporary file, which
	// changes with every attempt.
	msg := ""
	for inner := err; inner != nil; inner = errors.Unwrap(inner) {
		msg = inner.Error()
	}
	if msg == s.lastErr {
		return
	}
	s.lastErr = msg
	if err != nil {
		s.logf("careful resume: state file %s not written: %v", s.path, err)
	} else {
		s.logf("careful resume: state file %s written again", s.path)
	}
}

func (s *CarefulResumeStore) logf(format string, a ...any) {
	if s.log != nil {
		s.log(format, a...)
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
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.Version != carefulResumeStateVersion {
		return nil
	}
	now := s.now()
	for key, se := range state.Entries {
		if len(s.entries) >= carefulResumeMaxEntries {
			break
		}
		e := &carefulResumeEntry{
			params: CarefulResumeParams{
				Bandwidth: Bandwidth(se.BandwidthBPS),
				MinRTT:    time.Duration(se.MinRTTMicros) * time.Microsecond,
				CWND:      congestion.ByteCount(se.CWND),
			},
			savedAt: se.SavedAt,
		}
		if s.expired(e, now) || se.BandwidthBPS == 0 || se.MinRTTMicros <= 0 {
			continue
		}
		s.entries[key] = e
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
	s.sweepLocked(s.now())
	for key, e := range s.entries {
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
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
