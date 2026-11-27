package hysteria2

import "testing"

func TestConnectionStatsWithoutConnection(t *testing.T) {
	t.Parallel()
	client := &Client{}
	stats, connected := client.ConnectionStats()
	if connected {
		t.Fatal("a client that never connected reports a connection")
	}
	if stats.PacketsSent != 0 || stats.PacketsReceived != 0 {
		t.Fatal("statistics without a connection are not zero")
	}
}
