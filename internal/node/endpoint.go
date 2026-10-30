package node

import (
	"fmt"
	"net/url"
	"strings"
)

// Endpoint is how a client reaches a running node.
type Endpoint struct {
	Address string
	Port    int
	SNI     string
	Keys    Keys
}

// Link renders the endpoint in the de-facto standard vless:// share format,
// for phone apps (v2rayNG, Hiddify, Happ, Streisand...) via a QR code.
func (e Endpoint) Link(name string) string {
	q := url.Values{}
	q.Set("encryption", "none")
	q.Set("security", "reality")
	q.Set("sni", e.SNI)
	q.Set("fp", Fingerprint)
	q.Set("pbk", e.Keys.PublicKey)
	q.Set("sid", e.Keys.ShortID)
	q.Set("type", "tcp")
	q.Set("headerType", "none")
	host := e.Address
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("vless://%s@%s:%d?%s#%s", e.Keys.PhoneUUID(), host, e.Port, q.Encode(), url.PathEscape(name))
}
