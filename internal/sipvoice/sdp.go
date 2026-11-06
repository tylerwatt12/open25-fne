package sipvoice

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

const (
	PayloadType = 100
	ClockRate   = 8000
	maxSDPBytes = 4096
)

var (
	ErrSDP     = errors.New("invalid P25 SDP")
	ErrProfile = errors.New("unsupported P25 profile")
)

type Endpoints struct{ RTP, RTCP netip.AddrPort }

func (e Endpoints) valid() bool {
	a, p := e.RTP.Addr(), e.RTP.Port()
	return a.Is4() && !a.IsUnspecified() && !a.IsMulticast() && a != netip.AddrFrom4([4]byte{255, 255, 255, 255}) && p >= 2 && p <= 65532 && p%2 == 0 && e.RTCP == netip.AddrPortFrom(a, p+1)
}

// This private profile uses current FullRate over plain RTP inside the VPN.
// VBB1 is a fixed RTP payload contract, not an invented SDP fmtp parameter.
func marshalSDP(m Endpoints, id uint64) ([]byte, error) {
	if !m.valid() {
		return nil, ErrSDP
	}
	d := sdp.SessionDescription{
		Version:               0,
		Origin:                sdp.Origin{Username: "-", SessionID: id, SessionVersion: 0, NetworkType: "IN", AddressType: "IP4", UnicastAddress: m.RTP.Addr().String()},
		SessionName:           "TIA-P25-GroupCall",
		ConnectionInformation: &sdp.ConnectionInformation{NetworkType: "IN", AddressType: "IP4", Address: &sdp.Address{Address: m.RTP.Addr().String()}},
		TimeDescriptions:      []sdp.TimeDescription{{Timing: sdp.Timing{StartTime: 0, StopTime: 0}}},
		MediaDescriptions: []*sdp.MediaDescription{{
			MediaName: sdp.MediaName{Media: "audio", Port: sdp.RangedPort{Value: int(m.RTP.Port())}, Protos: []string{"RTP", "AVP"}, Formats: []string{"100"}},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "100 X-TIA-P25-FullRate/8000"},
				{Key: "sendrecv"},
			},
		}},
	}
	out, e := d.Marshal()
	if e != nil {
		return nil, ErrSDP
	}
	return out, nil
}

func parseSDP(raw []byte) (Endpoints, error) {
	var out Endpoints
	if len(raw) == 0 || len(raw) > maxSDPBytes {
		return out, ErrSDP
	}
	var d sdp.SessionDescription
	if d.Unmarshal(raw) != nil {
		return out, ErrSDP
	} // Parser errors can include untrusted packet contents.
	if d.Origin.NetworkType != "IN" || d.Origin.AddressType != "IP4" {
		return out, ErrProfile
	}
	if origin, e := netip.ParseAddr(d.Origin.UnicastAddress); e != nil || !origin.Is4() || origin.IsUnspecified() || origin.IsMulticast() || origin == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return out, ErrSDP
	}
	if d.Version != 0 || d.SessionName != "TIA-P25-GroupCall" || len(d.MediaDescriptions) != 1 || len(d.TimeDescriptions) != 1 || d.TimeDescriptions[0].Timing.StartTime != 0 || d.TimeDescriptions[0].Timing.StopTime != 0 || len(d.TimeDescriptions[0].RepeatTimes) != 0 || d.EncryptionKey != nil || len(d.Attributes) != 0 || len(d.TimeZones) != 0 {
		return out, ErrProfile
	}
	md := d.MediaDescriptions[0]
	if md.MediaName.Media != "audio" || strings.Join(md.MediaName.Protos, "/") != "RTP/AVP" || len(md.MediaName.Formats) != 1 || md.MediaName.Formats[0] != "100" || md.MediaName.Port.Range != nil || md.EncryptionKey != nil {
		return out, ErrProfile
	}
	ci := d.ConnectionInformation
	if md.ConnectionInformation != nil {
		if ci != nil {
			return out, ErrSDP
		}
		ci = md.ConnectionInformation
	}
	if ci == nil || ci.NetworkType != "IN" || ci.AddressType != "IP4" || ci.Address == nil || ci.Address.TTL != nil || ci.Address.Range != nil {
		return out, ErrSDP
	}
	a, e := netip.ParseAddr(ci.Address.Address)
	if e != nil {
		return out, ErrSDP
	}
	p := md.MediaName.Port.Value
	if p < 2 || p > 65532 {
		return out, ErrSDP
	}
	out = Endpoints{netip.AddrPortFrom(a, uint16(p)), netip.AddrPortFrom(a, uint16(p+1))}
	if !out.valid() {
		return out, ErrSDP
	}
	seen := map[string]bool{}
	for _, at := range md.Attributes {
		if seen[at.Key] {
			return Endpoints{}, ErrSDP
		}
		seen[at.Key] = true
		switch at.Key {
		case "rtpmap":
			if at.Value != "100 X-TIA-P25-FullRate/8000" {
				return Endpoints{}, ErrProfile
			}
		case "sendrecv":
			if at.Value != "" {
				return Endpoints{}, ErrSDP
			}
		case "rtcp":
			// RFC3605 explicit RTCP is accepted only at the paired endpoint; no mux.
			parts := strings.Fields(at.Value)
			if len(parts) != 1 && len(parts) != 4 {
				return Endpoints{}, ErrSDP
			}
			rp, err := strconv.Atoi(parts[0])
			if err != nil || rp != p+1 {
				return Endpoints{}, ErrSDP
			}
			if len(parts) == 4 && (parts[1] != "IN" || parts[2] != "IP4" || parts[3] != a.String()) {
				return Endpoints{}, ErrSDP
			}
		default:
			return Endpoints{}, ErrProfile
		}
	}
	if !seen["rtpmap"] {
		return Endpoints{}, ErrProfile
	}
	return out, nil
}
