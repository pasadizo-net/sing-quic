package hysteria2

import (
	"github.com/sagernet/quic-go"
)

// ConnectionStats returns the statistics of the client's QUIC connection:
// round trip times, the congestion window, and the bytes and packets sent,
// received and lost. It reports false when the client has no connection that
// is open, which is the case before its first use, after an idle close and
// after a failure; the next use of the client then connects again.
//
// The counters belong to one connection and start again at zero on the next
// one. Packets that are sent while none are received mean that the path is
// dead: QUIC acknowledges every packet that asks for it within its
// acknowledgment delay, and the keep alive of an idle connection is such a
// packet.
func (c *Client) ConnectionStats() (quic.ConnectionStats, bool) {
	c.connAccess.Lock()
	conn := c.conn
	c.connAccess.Unlock()
	if conn == nil || conn.quicConn == nil || !conn.active() {
		return quic.ConnectionStats{}, false
	}
	return conn.quicConn.ConnectionStats(), true
}
