package tunnel

import "testing"

func TestVPNLike(t *testing.T) {
	for name, want := range map[string]bool{
		"happ-tun": true, "AmneziaVPN": true, "MozillaVPN": true, "OpenVPN TAP-Windows6": true, "wg0": false,
		"minivpn": false, "Ethernet": false, "vEthernet (WSL)": false, "VMware Network Adapter VMnet8": false,
		"Teredo Tunneling Pseudo-Interface": false, "Bluetooth Network Connection": false,
	} {
		if got := vpnLike(name); got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
}

func TestOtherVPNsRuns(t *testing.T) {
	t.Logf("other VPNs here: %v", OtherVPNs())
}
