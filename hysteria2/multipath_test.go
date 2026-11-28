package hysteria2

import (
	"testing"

	N "github.com/sagernet/sing/common/network"
)

func TestCheckMultipathOptions(t *testing.T) {
	t.Parallel()
	paths := []MultipathPath{{Name: "lte", Dialer: N.SystemDialer, Backup: true}}
	for _, test := range []struct {
		name    string
		options ClientOptions
		ok      bool
	}{
		{"no paths", ClientOptions{ChromeParrot: true, ServerPorts: []string{"1000:2000"}}, true},
		{"paths", ClientOptions{MultipathPaths: paths}, true},
		{"port hopping", ClientOptions{MultipathPaths: paths, ServerPorts: []string{"1000:2000"}}, false},
		{"chrome parrot", ClientOptions{MultipathPaths: paths, ChromeParrot: true}, false},
		{"missing dialer", ClientOptions{MultipathPaths: []MultipathPath{{Name: "lte"}}}, false},
	} {
		if err := checkMultipathOptions(test.options); (err == nil) != test.ok {
			t.Errorf("%s: %v", test.name, err)
		}
	}
}
