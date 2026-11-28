package hysteria2

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/sagernet/quic-go"
	"github.com/sagernet/quic-go/congestion"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"
)

// A MultipathPath is a network path the client adds to its QUIC connection with the
// multipath extension of RFC 10069, besides the path its Dialer opens: for example a
// dialer bound to a second network interface. The server needs ServiceOptions.Multipath.
type MultipathPath struct {
	// Name identifies the path in logs.
	Name   string
	Dialer N.Dialer
	// Backup marks the path as a backup path: neither endpoint sends on it while
	// another path works.
	Backup bool
	// SendBPS is the bandwidth Brutal sends at on this path, if the client uses Brutal.
	// If zero, the client's SendBPS is used, as on the first path.
	SendBPS uint64
}

const (
	multipathOpenTimeout = 10 * time.Second
	// A path that failed is opened again after this delay, doubled on each failure up to the maximum.
	multipathRetryDelay    = 2 * time.Second
	multipathRetryDelayMax = time.Minute
)

func checkMultipathOptions(options ClientOptions) error {
	if len(options.MultipathPaths) == 0 {
		return nil
	}
	switch {
	case len(options.ServerPorts) > 0:
		return E.New("multipath and port hopping are mutually exclusive")
	case options.RealmOptions != nil:
		return E.New("multipath and realm are mutually exclusive")
	case options.ChromeParrot:
		return E.New("multipath and chrome_parrot are mutually exclusive")
	}
	for i, path := range options.MultipathPaths {
		if path.Dialer == nil {
			return E.New("multipath path ", i, ": missing dialer")
		}
	}
	return nil
}

// openPaths keeps the configured paths open on the connection while it is open.
// newCC creates the congestion controller of a path from its send bandwidth.
func (c *Client) openPaths(conn *clientQUICConnection, newCC func(sendBPS uint64) congestion.CongestionControl) {
	for _, path := range c.multipathPaths {
		go c.keepPathOpen(conn, path, newCC)
	}
}

func (c *Client) keepPathOpen(conn *clientQUICConnection, path MultipathPath, newCC func(sendBPS uint64) congestion.CongestionControl) {
	ctx := conn.quicConn.Context()
	delay := multipathRetryDelay
	for {
		opened, err := c.openPath(ctx, conn.quicConn, path, newCC(path.SendBPS))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, quic.ErrMultipathNotNegotiated) {
				c.logger.Warn("multipath: the server does not support multipath, path ", path.Name, " is not used")
				return
			}
			c.logger.Debug("multipath: open path ", path.Name, ": ", err)
		} else {
			c.logger.Debug("multipath: opened path ", path.Name, " as path ", opened.ID())
			delay = multipathRetryDelay
			select {
			case <-opened.Done():
				c.logger.Debug("multipath: path ", path.Name, " closed: ", opened.Err())
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = min(2*delay, multipathRetryDelayMax)
	}
}

func (c *Client) openPath(ctx context.Context, quicConn *quic.Conn, path MultipathPath, cc congestion.CongestionControl) (*quic.MultipathPath, error) {
	ctx, cancel := context.WithTimeout(ctx, multipathOpenTimeout)
	defer cancel()
	udpConn, err := path.Dialer.DialContext(ctx, N.NetworkUDP, c.serverAddr)
	if err != nil {
		return nil, err
	}
	return quicConn.OpenPathConn(ctx, c.obfuscate(udpConn), &quic.PathOptions{Backup: path.Backup, CongestionControl: cc})
}

// obfuscate wraps a socket of the client in the configured obfuscation.
func (c *Client) obfuscate(udpConn net.Conn) net.Conn {
	if c.geckoPassword != "" {
		return NewGeckoClientConn(udpConn, []byte(c.geckoPassword), c.geckoMinPacketSize, c.geckoMaxPacketSize)
	}
	if c.salamanderPassword != "" {
		return NewSalamanderClientConn(udpConn, []byte(c.salamanderPassword))
	}
	return udpConn
}
