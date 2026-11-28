package congestion

import (
	"fmt"
	"time"

	"github.com/sagernet/quic-go/congestion"
	"github.com/sagernet/quic-go/monotime"
)

// Careful Resume (RFC 9959) for BBR, following Appendix C of the RFC: a new
// connection to a peer that an earlier connection observed starts from half
// of the bottleneck bandwidth seen then, instead of from slow start, once the
// path has been confirmed, and falls back safely when the jump meets
// congestion.

type carefulResumePhase uint8

const (
	// Careful Resume is not, or no longer, in use: normal CC (Section 3.7).
	carefulResumeNormal carefulResumePhase = iota
	carefulResumeReconnaissance
	carefulResumeUnvalidated
	carefulResumeValidating
	carefulResumeSafeRetreat
)

func (p carefulResumePhase) String() string {
	switch p {
	case carefulResumeReconnaissance:
		return "RECONNAISSANCE"
	case carefulResumeUnvalidated:
		return "UNVALIDATED"
	case carefulResumeValidating:
		return "VALIDATING"
	case carefulResumeSafeRetreat:
		return "SAFE_RETREAT"
	default:
		return "NORMAL"
	}
}

const (
	// Beta, the scale of the PipeSize on leaving Safe Retreat (Section 3.5).
	carefulResumeBeta = 0.5
	// A current RTT more than this many times saved_rtt is a path change
	// (Section 4.2.1).
	carefulResumeMaxRTTFactor = 10
	// Parameters whose saved_cwnd is below this many initial windows are not
	// worth saving (Section 3.1).
	carefulResumeMinSavedIWs = 4
	// Careful Resume treats loss as congestion when, since the connection
	// started, at least carefulResumeMinLostPackets packets and more than
	// carefulResumeLossThreshold of the bytes acknowledged or lost were lost.
	// RFC 9959 names packet loss as an example of congestion; on the lossy
	// paths this is built for, a few per cent of the packets are lost at
	// random whatever the sender does, and BBR itself does not treat such loss
	// as congestion (it leaves STARTUP on loss only after 8 loss events and
	// above 2 % in a round). A jump into a bottleneck that has shrunk loses
	// far more than this within one round trip.
	carefulResumeLossThreshold  = 0.05
	carefulResumeMinLostPackets = 8
)

type carefulResume struct {
	store *CarefulResumeStore
	key   string
	claim uint64
	saved CarefulResumeParams
	phase carefulResumePhase
	// log, if not nil, reports the phase changes.
	log func(format string, a ...any)

	// The bytes acknowledged and lost since the connection started, to tell
	// congestion from random loss.
	bytesAcked  congestion.ByteCount
	bytesLost   congestion.ByteCount
	packetsLost int

	// Reconnaissance: the initial data and whether its ACK confirmed the path.
	initialBytesSent  congestion.ByteCount
	initialLastPacket congestion.PacketNumber
	pathConfirmed     bool

	// Unvalidated, Validating and Safe Retreat.
	pipeSize         congestion.ByteCount
	jumpWindow       congestion.ByteCount
	probeRate        Bandwidth
	unvalidatedStart monotime.Time // when the first unvalidated packet was sent
	unvalidatedRTT   time.Duration
	firstUnvalidated congestion.PacketNumber
	lastUnvalidated  congestion.PacketNumber
	lastCareful      congestion.PacketNumber
	validatingWindow congestion.ByteCount
	retreatWindow    congestion.ByteCount

	// The phases this connection went through, in order.
	phases []carefulResumePhase
}

func newCarefulResume(store *CarefulResumeStore, key string, log func(format string, a ...any)) *carefulResume {
	c := &carefulResume{
		store:             store,
		key:               key,
		log:               log,
		initialLastPacket: invalidPacketNumber,
		firstUnvalidated:  invalidPacketNumber,
		lastUnvalidated:   invalidPacketNumber,
		lastCareful:       invalidPacketNumber,
	}
	if params, id, ok := store.claim(key); ok {
		c.saved = params
		c.claim = id
		c.setPhase(carefulResumeReconnaissance)
		c.logf("RECONNAISSANCE for %s, saved %s at %s", key, formatMbps(params.Bandwidth), params.MinRTT)
	}
	return c
}

func (c *carefulResume) onPacketSent(b *bbrSender, sentTime monotime.Time, bytesInFlight congestion.ByteCount, packetNumber congestion.PacketNumber, bytes congestion.ByteCount) {
	switch c.phase {
	case carefulResumeReconnaissance:
		// The initial data: what the initial window allows in the first RTT.
		if !c.pathConfirmed && c.initialBytesSent < b.initialCongestionWindow {
			c.initialBytesSent += bytes
			c.initialLastPacket = packetNumber
		}
	case carefulResumeUnvalidated:
		if c.firstUnvalidated == invalidPacketNumber {
			c.firstUnvalidated = packetNumber
			c.unvalidatedStart = sentTime
		}
		c.lastUnvalidated = packetNumber
		c.lastCareful = packetNumber
		// Completed sending all unvalidated packets: the jump window is full.
		if bytesInFlight+b.maxDatagramSize > c.jumpWindow {
			c.endUnvalidated(b, bytesInFlight, "last_unvalidated_packet_sent")
		} else if sentTime.Sub(c.unvalidatedStart) > c.unvalidatedRTT {
			c.endUnvalidated(b, bytesInFlight, "rtt_exceeded")
		}
	case carefulResumeValidating:
		c.lastCareful = packetNumber
	}
}

// maybeJump enters the Unvalidated Phase when the path is confirmed and the
// sender has more data than the congestion window allows: the transition is
// deferred to that moment, as Section 3.2 permits, so that a sender that is
// rate limited does not jump.
func (c *carefulResume) maybeJump(b *bbrSender, bytesInFlight, congestionWindow congestion.ByteCount) {
	if c.phase != carefulResumeReconnaissance || !c.pathConfirmed || bytesInFlight < congestionWindow {
		return
	}
	// Unvalidated packets are sent only in STARTUP (Appendix C.1).
	if b.mode != bbrModeStartup || b.isAtFullBandwidth {
		c.stop("not_in_startup")
		return
	}
	// The lifetime may have run out, or the lease passed to another
	// connection, while this one was rate limited.
	if !c.store.holds(c.key, c.claim) {
		c.stop("saved_parameters_gone")
		return
	}
	jump := min(c.saved.CWND/2, b.maxCongestionWindow)
	if jump <= congestionWindow {
		c.stop("jump_not_larger")
		return
	}
	rtt := b.rttStats.SmoothedRTT()
	if rtt == 0 {
		rtt = b.getMinRtt()
	}
	c.setPhase(carefulResumeUnvalidated)
	c.pipeSize = bytesInFlight
	c.jumpWindow = jump
	// Pace the unvalidated packets at half the saved bottleneck bandwidth
	// (Section 1.5 and Appendix C.1), and never faster than the jump window
	// per current RTT (Section 4.3.2).
	c.probeRate = min(c.saved.Bandwidth/2, BandwidthFromDelta(jump, rtt))
	c.unvalidatedRTT = rtt
	c.logf("UNVALIDATED, jump %d bytes, probe %s, rtt %s", jump, formatMbps(c.probeRate), rtt)
}

func (c *carefulResume) onCongestionEvent(b *bbrSender, eventTime monotime.Time, ackedPackets []congestion.AckedPacketInfo, lostPackets []congestion.LostPacketInfo) {
	var ackedBytes, lostBytes congestion.ByteCount
	largestAcked := congestion.PacketNumber(invalidPacketNumber)
	for _, p := range ackedPackets {
		ackedBytes += p.BytesAcked
		largestAcked = max(largestAcked, p.PacketNumber)
	}
	for _, p := range lostPackets {
		lostBytes += p.BytesLost
	}
	c.bytesAcked += ackedBytes
	c.bytesLost += lostBytes
	c.packetsLost += len(lostPackets)
	congested := len(lostPackets) != 0 &&
		c.packetsLost >= carefulResumeMinLostPackets &&
		float64(c.bytesLost) > carefulResumeLossThreshold*float64(c.bytesAcked+c.bytesLost)

	switch c.phase {
	case carefulResumeReconnaissance:
		if congested {
			c.stop("packet_loss")
			return
		}
		if c.pathConfirmed || c.initialLastPacket == invalidPacketNumber || largestAcked < c.initialLastPacket {
			return
		}
		// All the initial data is acknowledged: confirm the path by its RTT
		// (Section 4.2.1).
		rtt := b.getMinRtt()
		if rtt <= c.saved.MinRTT/2 {
			c.stop("rtt_not_validated")
			return
		}
		if rtt > carefulResumeMaxRTTFactor*c.saved.MinRTT {
			c.stop("path_changed")
			return
		}
		c.pathConfirmed = true
		c.logf("path confirmed, min rtt %s", rtt)
	case carefulResumeUnvalidated:
		c.pipeSize += ackedBytes
		switch {
		case congested:
			c.enterSafeRetreat(b, "packet_loss")
		case b.rttStats.LatestRTT() > carefulResumeMaxRTTFactor*c.saved.MinRTT:
			c.enterSafeRetreat(b, "path_changed")
		case c.firstUnvalidated != invalidPacketNumber && largestAcked >= c.firstUnvalidated:
			c.endUnvalidated(b, b.bytesInFlight, "first_unvalidated_packet_acknowledged")
		case c.firstUnvalidated != invalidPacketNumber && eventTime.Sub(c.unvalidatedStart) > c.unvalidatedRTT:
			c.endUnvalidated(b, b.bytesInFlight, "rtt_exceeded")
		}
	case carefulResumeValidating:
		c.pipeSize += ackedBytes
		if congested {
			c.enterSafeRetreat(b, "packet_loss")
			return
		}
		// The CWND grows with the ACKs, as in slow start (Section 3.4).
		c.validatingWindow += ackedBytes
		if largestAcked >= c.lastUnvalidated {
			b.congestionWindow = min(max(b.congestionWindow, c.validatingWindow), b.maxCongestionWindow)
			c.stop("last_unvalidated_packet_acknowledged")
		}
	case carefulResumeSafeRetreat:
		c.pipeSize += ackedBytes
		if largestAcked >= c.lastCareful {
			c.exitSafeRetreat(b, eventTime)
		}
	}
}

// endUnvalidated leaves the Unvalidated Phase (Section 3.3, "check
// flight_size"): back to normal CC when the sender did not use the jump,
// else on to the Validating Phase with the CWND set to the flight size.
func (c *carefulResume) endUnvalidated(b *bbrSender, flightSize congestion.ByteCount, reason string) {
	if flightSize < b.initialCongestionWindow || flightSize <= c.pipeSize {
		b.congestionWindow = min(max(b.congestionWindow, c.pipeSize), b.maxCongestionWindow)
		c.stop("rate_limited, " + reason)
		return
	}
	c.setPhase(carefulResumeValidating)
	c.validatingWindow = flightSize
	c.logf("VALIDATING (%s), flight %d, pipe %d", reason, flightSize, c.pipeSize)
}

// enterSafeRetreat reacts to congestion of unvalidated packets (Section 3.5
// and Appendix C.3): the saved parameters are deleted, the CWND falls to half
// the PipeSize and is not raised until the unvalidated packets are
// acknowledged, and BBR leaves STARTUP for DRAIN with the delivery rates
// measured during the jump discarded.
func (c *carefulResume) enterSafeRetreat(b *bbrSender, reason string) {
	c.store.forget(c.key)
	c.claim = 0
	c.setPhase(carefulResumeSafeRetreat)
	c.retreatWindow = max(b.minCongestionWindow, c.pipeSize/2)
	b.maxBandwidth.Reset(BandwidthFromDelta(c.retreatWindow, b.getMinRtt()), b.roundTripCount)
	b.congestionWindow = min(b.congestionWindow, c.retreatWindow)
	if b.mode == bbrModeStartup {
		b.isAtFullBandwidth = true
		b.mode = bbrModeDrain
		b.pacingGain = b.drainGain
		b.congestionWindowGain = b.highCwndGain
	}
	c.logf("SAFE_RETREAT (%s), pipe %d, cwnd %d", reason, c.pipeSize, c.retreatWindow)
}

// exitSafeRetreat returns to normal CC once the last packet sent while
// resuming is acknowledged. The PipeSize times Beta becomes the new
// bandwidth estimate, the BBR counterpart of setting ssthresh (Section 3.5):
// the bytes delivered since the jump over the time they took.
func (c *carefulResume) exitSafeRetreat(b *bbrSender, now monotime.Time) {
	elapsed := b.getMinRtt()
	if c.firstUnvalidated != invalidPacketNumber {
		elapsed = max(elapsed, now.Sub(c.unvalidatedStart))
	}
	bw := BandwidthFromDelta(congestion.ByteCount(float64(c.pipeSize)*carefulResumeBeta), elapsed)
	b.maxBandwidth.Reset(bw, b.roundTripCount)
	c.setPhase(carefulResumeNormal)
	c.logf("NORMAL (exit_recovery), pipe %d, bandwidth %s", c.pipeSize, formatMbps(bw))
}

// stop leaves Careful Resume for normal CC and lets other connections use
// the saved parameters again.
func (c *carefulResume) stop(reason string) {
	if c.claim != 0 {
		c.store.release(c.key, c.claim)
		c.claim = 0
	}
	c.setPhase(carefulResumeNormal)
	c.logf("NORMAL (%s)", reason)
}

func (c *carefulResume) setPhase(phase carefulResumePhase) {
	c.phase = phase
	c.phases = append(c.phases, phase)
}

func (c *carefulResume) logf(format string, a ...any) {
	if c.log != nil {
		c.log("CarefulResume: "+format, a...)
	}
}

// discardBandwidthSamples reports whether the delivery rates measured now are
// tainted by a failed jump and must not reach the bandwidth filter.
func (c *carefulResume) discardBandwidthSamples() bool {
	return c.phase == carefulResumeSafeRetreat
}

func (c *carefulResume) congestionWindow(cwnd congestion.ByteCount) congestion.ByteCount {
	switch c.phase {
	case carefulResumeUnvalidated:
		return max(cwnd, c.jumpWindow)
	case carefulResumeValidating:
		return max(cwnd, c.validatingWindow)
	case carefulResumeSafeRetreat:
		return min(cwnd, c.retreatWindow)
	default:
		return cwnd
	}
}

func (c *carefulResume) pacingRate(rate Bandwidth) Bandwidth {
	switch c.phase {
	case carefulResumeUnvalidated, carefulResumeValidating:
		return max(rate, c.probeRate)
	default:
		return rate
	}
}

// observe saves the path's capacity once BBR has a steady estimate of it
// (Sections 3.1 and 4.1): in PROBE_BW, never while resuming, and never from
// the overshoot of STARTUP.
func (c *carefulResume) observe(b *bbrSender) {
	if c.phase != carefulResumeNormal || b.mode != bbrModeProbeBw || b.minRtt == 0 {
		return
	}
	bw := b.bandwidthEstimate()
	cwnd := bdpFromRttAndBandwidth(b.minRtt, bw)
	if cwnd < carefulResumeMinSavedIWs*b.initialCongestionWindow {
		return
	}
	c.store.observe(c.key, CarefulResumeParams{
		Bandwidth: bw,
		MinRTT:    b.minRtt,
		CWND:      cwnd,
	}, b.lastSampleIsAppLimited)
}

func formatMbps(bw Bandwidth) string {
	return fmt.Sprintf("%.2f Mbit/s", float64(bw)/1e6)
}
