module github.com/sagernet/sing-quic

go 1.27.4

require (
	github.com/gofrs/uuid/v5 v5.5.1
	github.com/libp2p/go-nat v1.0.1-0.20250821073202-01afc089f138
	github.com/sagernet/quic-go v0.66.0-sing-box-mod.1
	github.com/sagernet/sing v0.9.6
	golang.org/x/crypto v0.59.0
	golang.org/x/exp v0.0.0-20261128033645-f63ec7c7eed0
	golang.org/x/sync v0.25.0
)

require (
	github.com/andybalholm/brotli v1.3.0 // indirect
	github.com/huin/goupnp v1.3.1 // indirect
	github.com/jackpal/go-nat-pmp v1.1.0 // indirect
	github.com/klauspost/compress v1.20.5 // indirect
	github.com/koron/go-ssdp v0.9.2 // indirect
	github.com/libp2p/go-netroute v0.4.0 // indirect
	github.com/metacubex/utls v1.8.8 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/net v0.63.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.44.0 // indirect
)

replace github.com/sagernet/quic-go => github.com/pasadizo-net/quic-go v0.0.0-20261128121951-bb78f72c1784
