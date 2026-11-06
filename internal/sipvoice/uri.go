package sipvoice

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

func peerUser(peerID string) string {
	const hex = "0123456789ABCDEF"
	var value strings.Builder
	for i := range len(peerID) {
		c := peerID[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			value.WriteByte(c)
		} else {
			value.WriteByte('%')
			value.WriteByte(hex[c>>4])
			value.WriteByte(hex[c&15])
		}
	}
	return value.String()
}
func canonicalPeerUser(value string) bool {
	decoded, err := url.PathUnescape(value)
	return err == nil && token(decoded, 128) && peerUser(decoded) == value
}

// PeerURI preserves an enrolled identity in the SIP user field. sipgo retains
// percent escapes; a raw colon would instead delimit a URI password.
func PeerURI(peerID, address string) (string, error) {
	endpoint, err := netip.ParseAddrPort(address)
	if !token(peerID, 128) || err != nil || !endpoint.Addr().Is4() || endpoint.Addr().IsUnspecified() || endpoint.Addr().IsMulticast() || endpoint.Addr() == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || endpoint.Port() == 0 {
		return "", errors.New("SIP peer needs an enrolled identity and explicit IPv4 endpoint")
	}
	return "sip:" + peerUser(peerID) + "@" + endpoint.Addr().String() + ":" + strconv.Itoa(int(endpoint.Port())) + ";transport=tcp", nil
}
