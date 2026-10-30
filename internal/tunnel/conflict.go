package tunnel

import (
	"net"
	"strings"
)

// InterfaceName is the TUN adapter sing-box creates for minivpn.
const InterfaceName = "minivpn"

// vpnWords pick VPN adapters out by name: wintun and TAP adapters are named
// by the app that creates them (happ-tun, AmneziaVPN, MozillaVPN, ...).
var vpnWords = []string{"tun", "wireguard", "vpn", "tap-", "amnezia", "outline", "hiddify", "happ", "clash", "nekoray", "v2ray", "xray", "sing-box"}

func vpnLike(name string) bool {
	n := strings.ToLower(name)
	if n == InterfaceName || strings.Contains(n, "teredo") || strings.Contains(n, "isatap") {
		return false
	}
	for _, w := range vpnWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// OtherVPNs lists adapters of other VPNs that are up with an IPv4 address.
// Two TUNs fight over the routes, and the node checks would go through the
// other VPN's server instead of the user's own network.
func OtherVPNs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var found []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || !vpnLike(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil {
				found = append(found, ifc.Name)
				break
			}
		}
	}
	return found
}
