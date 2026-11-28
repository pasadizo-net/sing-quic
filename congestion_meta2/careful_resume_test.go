package congestion

import (
	"math"
	"math/rand"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sagernet/quic-go/congestion"
	"github.com/sagernet/quic-go/monotime"
)

// A small packet-level simulation of one sender behind one bottleneck: a
// link with a rate, a one-way delay, a drop-tail queue and random loss.

const simPacketSize = congestion.ByteCount(1200)

type simClock struct{ now monotime.Time }

type simRTTStats struct {
	min, latest, smoothed, meanDev time.Duration
}

func (r *simRTTStats) MinRTT() time.Duration        { return r.min }
func (r *simRTTStats) LatestRTT() time.Duration     { return r.latest }
func (r *simRTTStats) SmoothedRTT() time.Duration   { return r.smoothed }
func (r *simRTTStats) MeanDeviation() time.Duration { return r.meanDev }
func (r *simRTTStats) MaxAckDelay() time.Duration   { return 0 }
func (r *simRTTStats) PTO(bool) time.Duration       { return r.smoothed + 4*r.meanDev }
func (r *simRTTStats) SetMaxAckDelay(time.Duration) {}
func (r *simRTTStats) SetInitialRTT(time.Duration)  {}

func (r *simRTTStats) UpdateRTT(sample, _ time.Duration) {
	r.latest = sample
	if r.min == 0 || sample < r.min {
		r.min = sample
	}
	if r.smoothed == 0 {
		r.smoothed = sample
		r.meanDev = sample / 2
		return
	}
	diff := r.smoothed - sample
	if diff < 0 {
		diff = -diff
	}
	r.meanDev = (3*r.meanDev + diff) / 4
	r.smoothed = (7*r.smoothed + sample) / 8
}

type simLink struct {
	bytesPerSecond float64
	delay          time.Duration // one way
	queue          congestion.ByteCount
	loss           float64
}

type simPacket struct {
	number congestion.PacketNumber
	sent   monotime.Time
	due    monotime.Time // when the ACK, or the loss detection, arrives
	lost   bool
}

type simResult struct {
	delivered []congestion.ByteCount // per 100 ms
	sent      congestion.ByteCount
	lost      congestion.ByteCount
}

func (r simResult) deliveredBy(d time.Duration) congestion.ByteCount {
	var total congestion.ByteCount
	for i := 0; i < int(d/(100*time.Millisecond)) && i < len(r.delivered); i++ {
		total += r.delivered[i]
	}
	return total
}

type simulation struct {
	clock *simClock
	rand  *rand.Rand

	// The connection of the sender made last.
	pending  []*simPacket
	inFlight congestion.ByteCount
	next     congestion.PacketNumber
	linkFree monotime.Time
}

func newSimulation() *simulation {
	return &simulation{
		clock: &simClock{now: monotime.Time(time.Hour)},
		rand:  rand.New(rand.NewSource(1)),
	}
}

// newSender makes a BBR sender as UseBBR does, with Careful Resume when
// store is not nil.
func (s *simulation) newSender(store *CarefulResumeStore) (*bbrSender, *simRTTStats) {
	var b *bbrSender
	if store != nil {
		b = NewBbrSenderWithCarefulResume(simPacketSize, ProfileStandard, store, simPeer, nil)
	} else {
		b = NewBbrSenderWithProfile(simPacketSize, ProfileStandard)
	}
	rtt := &simRTTStats{}
	b.SetRTTStatsProvider(rtt)
	s.pending, s.inFlight, s.next, s.linkFree = nil, 0, 0, s.clock.now
	return b, rtt
}

var simPeer = &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 4433}

// run sends as fast as b allows over link for d. b must be the sender made
// last; a later run goes on with the same connection.
func (s *simulation) run(b *bbrSender, rtt *simRTTStats, link simLink, d time.Duration) simResult {
	const step = 100 * time.Microsecond
	start := s.clock.now
	end := start.Add(d)
	res := simResult{delivered: make([]congestion.ByteCount, d/(100*time.Millisecond)+1)}
	for ; s.clock.now < end; s.clock.now = s.clock.now.Add(step) {
		now := s.clock.now
		var acked []congestion.AckedPacketInfo
		var lost []congestion.LostPacketInfo
		prior := s.inFlight
		kept := s.pending[:0]
		for _, p := range s.pending {
			switch {
			case p.due > now:
				kept = append(kept, p)
			case p.lost:
				lost = append(lost, congestion.LostPacketInfo{PacketNumber: p.number, BytesLost: simPacketSize})
				s.inFlight -= simPacketSize
				res.lost += simPacketSize
			default:
				acked = append(acked, congestion.AckedPacketInfo{PacketNumber: p.number, BytesAcked: simPacketSize, ReceivedTime: now})
				s.inFlight -= simPacketSize
				rtt.UpdateRTT(now.Sub(p.sent), 0)
				res.delivered[now.Sub(start)/(100*time.Millisecond)] += simPacketSize
			}
		}
		s.pending = kept
		if len(acked) != 0 || len(lost) != 0 {
			b.OnCongestionEventEx(prior, now, acked, lost)
		}
		for b.CanSend(s.inFlight) {
			if t := b.TimeUntilSend(s.inFlight); t != 0 && t > now {
				break
			}
			if !b.HasPacingBudget(now) {
				break
			}
			s.inFlight += simPacketSize
			res.sent += simPacketSize
			b.OnPacketSent(now, s.inFlight, s.next, simPacketSize, true)
			p := &simPacket{number: s.next, sent: now}
			s.next++
			queued := congestion.ByteCount(float64(max(s.linkFree.Sub(now), 0)) / float64(time.Second) * link.bytesPerSecond)
			if queued > link.queue {
				// Dropped at the bottleneck; found lost about an RTT later.
				p.lost = true
				p.due = now.Add(2*link.delay + 10*time.Millisecond)
			} else {
				s.linkFree = max(s.linkFree, now).Add(time.Duration(float64(simPacketSize) / link.bytesPerSecond * float64(time.Second)))
				p.due = s.linkFree.Add(2 * link.delay)
				if s.rand.Float64() < link.loss {
					p.lost = true
					p.due = p.due.Add(10 * time.Millisecond)
				}
			}
			s.pending = append(s.pending, p)
		}
	}
	return res
}

var (
	// 200 Mbit/s and 100 ms RTT, the path from Iran to Helsinki.
	sim200 = simLink{bytesPerSecond: 25e6, delay: 50 * time.Millisecond, queue: 1 << 20}
	// The same path when the capacity has fallen to 20 Mbit/s.
	sim20 = simLink{bytesPerSecond: 2.5e6, delay: 50 * time.Millisecond, queue: 256 << 10}
)

func newTestStore(t *testing.T) *CarefulResumeStore {
	t.Helper()
	store, err := NewCarefulResumeStore(time.Hour, "")
	requireNoError(t, err)
	return store
}

func TestCarefulResumeObservesTheCapacity(t *testing.T) {
	sim := newSimulation()
	store := newTestStore(t)
	b, rtt := sim.newSender(store)
	sim.run(b, rtt, sim200, 10*time.Second)

	params, id, ok := store.claim(CarefulResumeKey(simPeer))
	requireTrue(t, ok)
	requireNotZero(t, id)
	requireInDelta(t, 200e6, float64(params.Bandwidth), 40e6)
	requireInDelta(t, float64(100*time.Millisecond), float64(params.MinRTT), float64(10*time.Millisecond))
	requireEqual(t, bdpFromRttAndBandwidth(params.MinRTT, params.Bandwidth), params.CWND)
	requireEqual(t, []carefulResumePhase(nil), b.carefulResume.phases)
}

func TestCarefulResumeSpeedsUpANewConnection(t *testing.T) {
	sim := newSimulation()
	store := newTestStore(t)
	first, rtt := sim.newSender(store)
	sim.run(first, rtt, sim200, 10*time.Second)

	plain, rtt := sim.newSender(nil)
	without := sim.run(plain, rtt, sim200, 3*time.Second)

	resumed, rtt := sim.newSender(store)
	with := sim.run(resumed, rtt, sim200, 3*time.Second)

	requireEqual(t, []carefulResumePhase{
		carefulResumeReconnaissance,
		carefulResumeUnvalidated,
		carefulResumeValidating,
		carefulResumeNormal,
	}, resumed.carefulResume.phases)
	t.Logf("delivered in 1 s: %d without, %d with Careful Resume", without.deliveredBy(time.Second), with.deliveredBy(time.Second))
	requireGreater(t, with.deliveredBy(time.Second), without.deliveredBy(time.Second)*5/4)
	// The saved parameters stay for the next connection.
	requireEqual(t, 1, store.Len())
}

func TestCarefulResumeRetreatsWhenTheCapacityHasFallen(t *testing.T) {
	sim := newSimulation()
	store := newTestStore(t)
	first, rtt := sim.newSender(store)
	sim.run(first, rtt, sim200, 10*time.Second)

	resumed, rtt := sim.newSender(store)
	res := sim.run(resumed, rtt, sim20, 400*time.Millisecond)

	requireEqual(t, []carefulResumePhase{
		carefulResumeReconnaissance,
		carefulResumeUnvalidated,
		carefulResumeValidating,
		carefulResumeSafeRetreat,
	}, resumed.carefulResume.phases[:4])
	// Safe Retreat deletes the saved parameters.
	_, _, ok := store.claim(CarefulResumeKey(simPeer))
	requireFalse(t, ok)

	// It then settles at the new capacity.
	res = sim.run(resumed, rtt, sim20, 5*time.Second)
	requireEqual(t, carefulResumeNormal, resumed.carefulResume.phase)
	requireInDelta(t, 2.5e6, float64(res.delivered[40]+res.delivered[41]+res.delivered[42]+res.delivered[43]+res.delivered[44])*2, 0.5e6)
}

func TestCarefulResumeIgnoresRandomLoss(t *testing.T) {
	sim := newSimulation()
	store := newTestStore(t)
	first, rtt := sim.newSender(store)
	sim.run(first, rtt, sim200, 10*time.Second)

	lossy := sim200
	lossy.loss = 0.01
	resumed, rtt := sim.newSender(store)
	sim.run(resumed, rtt, lossy, 3*time.Second)
	requireContains(t, resumed.carefulResume.phases, carefulResumeValidating)
	requireNotContains(t, resumed.carefulResume.phases, carefulResumeSafeRetreat)
}

func TestCarefulResumeNeedsTheSameRTT(t *testing.T) {
	sim := newSimulation()
	store := newTestStore(t)
	key := CarefulResumeKey(simPeer)
	// Saved on a path with three times the RTT: the current RTT is less than
	// half of it.
	store.observe(key, CarefulResumeParams{
		Bandwidth: 200e6,
		MinRTT:    300 * time.Millisecond,
		CWND:      bdpFromRttAndBandwidth(300*time.Millisecond, 200e6),
	}, false)

	resumed, rtt := sim.newSender(store)
	sim.run(resumed, rtt, sim200, time.Second)
	requireEqual(t, []carefulResumePhase{carefulResumeReconnaissance, carefulResumeNormal}, resumed.carefulResume.phases)
	// The parameters are not deleted, and the claim is released.
	_, _, ok := store.claim(key)
	requireTrue(t, ok)
}

func TestCarefulResumeStoreOneConnectionAtATime(t *testing.T) {
	store := newTestStore(t)
	now := time.Unix(1_800_000_000, 0)
	store.now = func() time.Time { return now }
	p := CarefulResumeParams{Bandwidth: 100e6, MinRTT: 100 * time.Millisecond, CWND: 1250000}
	store.observe("192.0.2.1", p, false)

	got, id, ok := store.claim("192.0.2.1")
	requireTrue(t, ok)
	requireEqual(t, p, got)
	_, _, ok = store.claim("192.0.2.1")
	requireFalse(t, ok, "a second connection must not use the same parameters")
	requireTrue(t, store.holds("192.0.2.1", id))

	// A claim that is never released runs out after the lease.
	now = now.Add(carefulResumeLease + time.Second)
	_, id2, ok := store.claim("192.0.2.1")
	requireTrue(t, ok)
	requireFalse(t, store.holds("192.0.2.1", id))
	store.release("192.0.2.1", id2)
	_, _, ok = store.claim("192.0.2.1")
	requireTrue(t, ok)
}

func TestCarefulResumeStoreLifetime(t *testing.T) {
	store, err := NewCarefulResumeStore(time.Minute, "")
	requireNoError(t, err)
	now := time.Unix(1_800_000_000, 0)
	store.now = func() time.Time { return now }
	store.observe("192.0.2.1", CarefulResumeParams{Bandwidth: 100e6, MinRTT: time.Millisecond, CWND: 12500}, false)
	now = now.Add(time.Minute + time.Second)
	_, _, ok := store.claim("192.0.2.1")
	requireFalse(t, ok)
	requireZero(t, store.Len())
}

func TestCarefulResumeStoreKeepsLargerObservationWhenAppLimited(t *testing.T) {
	store := newTestStore(t)
	big := CarefulResumeParams{Bandwidth: 100e6, MinRTT: 100 * time.Millisecond, CWND: 1250000}
	small := CarefulResumeParams{Bandwidth: 10e6, MinRTT: 100 * time.Millisecond, CWND: 125000}
	store.observe("192.0.2.1", big, false)
	store.observe("192.0.2.1", small, true)
	got, _, _ := store.claim("192.0.2.1")
	requireEqual(t, big, got)
	store.observe("192.0.2.1", small, false)
	got, _, _ = store.claim("192.0.2.1")
	requireEqual(t, CarefulResumeParams{}, got, "still claimed")
	store.release("192.0.2.1", 1)
	got, _, _ = store.claim("192.0.2.1")
	requireEqual(t, small, got)
}

func TestCarefulResumeStoreStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "careful-resume.json")
	store, err := NewCarefulResumeStore(time.Hour, path)
	requireNoError(t, err)
	p := CarefulResumeParams{Bandwidth: 160e6, MinRTT: 98 * time.Millisecond, CWND: 1960000}
	store.observe("2001:db8::1", p, false)
	requireNoError(t, store.Close())

	loaded, err := NewCarefulResumeStore(time.Hour, path)
	requireNoError(t, err)
	defer loaded.Close()
	got, _, ok := loaded.claim("2001:db8::1")
	requireTrue(t, ok)
	requireEqual(t, p, got)

	// Expired entries are not loaded.
	short, err := NewCarefulResumeStore(time.Nanosecond, path)
	requireNoError(t, err)
	defer short.Close()
	requireZero(t, short.Len())
}

func TestCarefulResumeKey(t *testing.T) {
	requireEqual(t, "192.0.2.1", CarefulResumeKey(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}))
	requireEqual(t, "192.0.2.1", CarefulResumeKey(&net.UDPAddr{IP: net.ParseIP("::ffff:192.0.2.1"), Port: 2}))
	requireEqual(t, "2001:db8::1", CarefulResumeKey(&net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 3}))
}

// Small stand-ins for testify, which this module does not use.

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func requireEqual(t *testing.T, want, got any, msg ...any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("want %v, got %v %v", want, got, msg)
	}
}

func requireTrue(t *testing.T, ok bool, msg ...any) {
	t.Helper()
	if !ok {
		t.Fatal("want true", msg)
	}
}

func requireFalse(t *testing.T, ok bool, msg ...any) {
	t.Helper()
	if ok {
		t.Fatal("want false", msg)
	}
}

func requireZero[T comparable](t *testing.T, v T) {
	t.Helper()
	var zero T
	if v != zero {
		t.Fatalf("want zero, got %v", v)
	}
}

func requireNotZero[T comparable](t *testing.T, v T) {
	t.Helper()
	var zero T
	if v == zero {
		t.Fatal("want not zero")
	}
}

func requireInDelta(t *testing.T, want, got, delta float64) {
	t.Helper()
	if math.Abs(want-got) > delta {
		t.Fatalf("want %v within %v, got %v", want, delta, got)
	}
}

func requireGreater(t *testing.T, a, b congestion.ByteCount) {
	t.Helper()
	if a <= b {
		t.Fatalf("want %d > %d", a, b)
	}
}

func requireContains(t *testing.T, phases []carefulResumePhase, p carefulResumePhase) {
	t.Helper()
	if !slices.Contains(phases, p) {
		t.Fatalf("%v does not contain %v", phases, p)
	}
}

func requireNotContains(t *testing.T, phases []carefulResumePhase, p carefulResumePhase) {
	t.Helper()
	if slices.Contains(phases, p) {
		t.Fatalf("%v contains %v", phases, p)
	}
}
