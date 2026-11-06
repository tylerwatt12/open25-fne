package dispatch

import (
	"errors"
	"open25.local/open25-fne/internal/p25crypto"
	"open25.local/open25-fne/internal/p25rtp"
)

const (
	opaqueFullRateOperation = 18
	packetFlagEnd           = 1
	packetFlagHeader        = 2
)

func fullRatePayload(f Frame) ([]byte, uint16, error) {
	if f.P25 == nil {
		return nil, 0, ErrP25MetadataNotSupported
	}
	m := f.P25
	index, ok := m.Type.Index()
	if !ok || index != f.VoiceIndex || len(f.Encoded) != 11 {
		return nil, 0, errors.New("P25 FullRate type, voice index or word length is invalid")
	}
	v := p25rtp.FullRateVoice{Type: m.Type, Quality: m.Quality, LSD: m.LSD, ES: m.ES}
	copy(v.IMBE[:], f.Encoded)
	b, err := p25rtp.MarshalVoice(v)
	if err != nil {
		return nil, 0, err
	}
	if m.Header == nil {
		return b, 0, nil
	}
	h := m.Header
	if h.GroupID != uint16(f.TalkgroupID) || h.AlgorithmID != f.AlgorithmID || (f.Encrypted && (h.KeyID != f.KeyID || !p25crypto.ValidMessageIndicator(h.MI[:]))) {
		return nil, 0, errors.New("P25 header does not match its media owner")
	}
	header, err := p25rtp.MarshalHeaderWord(*h)
	if err != nil {
		return nil, 0, err
	}
	return append(header, b...), packetFlagHeader, nil
}

func decodeFullRatePayload(f *Frame, b []byte, flags uint16) error {
	if len(b) == 11 {
		return ErrP25MetadataNotSupported
	}
	var header *p25rtp.HeaderWord
	if flags&packetFlagHeader != 0 {
		if len(b) < 18 {
			return errors.New("P25 FullRate header is truncated")
		}
		h, err := p25rtp.ParseHeaderWord(b[:18])
		if err != nil {
			return err
		}
		header = &h
		b = b[18:]
	}
	if len(b) == 11 {
		return ErrP25MetadataNotSupported
	}
	v, err := p25rtp.ParseVoice(b)
	if err != nil {
		return err
	}
	f.Encoded = append([]byte{}, v.IMBE[:]...)
	f.P25 = &P25VoiceMetadata{Type: v.Type, Quality: v.Quality, LSD: v.LSD, ES: v.ES, Header: header}
	_, _, err = fullRatePayload(*f)
	return err
}
