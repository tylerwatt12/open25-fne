package framework

import (
	"context"
	"errors"
	"open25.local/open25-fne/internal/dispatch"
	"time"
)

func (s *Service) decodeDispatchFrame(frame dispatch.Frame) (dispatch.Frame, error) {
	codec, _ := s.packetCodec()
	if s.crypto == nil {
		return dispatch.Frame{}, dispatch.ErrUnavailable
	}
	owner, err := s.crypto.receiveOwner(frame, codec, time.Now())
	if err != nil {
		return dispatch.Frame{}, err
	}
	if !s.currentPacketCodec(owner) {
		s.crypto.blockReceive(frame, owner)
		return dispatch.Frame{}, errAudioProcessChanged
	}
	ctx, cancel := context.WithTimeout(s.operationCtx, 200*time.Millisecond)
	decoded, err := owner.Decode(ctx, frame)
	cancel()
	if err != nil {
		if !s.currentPacketCodec(owner) {
			s.crypto.blockReceive(frame, owner)
		}
		return dispatch.Frame{}, err
	}
	if !s.currentPacketCodec(owner) || !s.crypto.receiveOwned(frame, owner) {
		s.crypto.blockReceive(frame, owner)
		return dispatch.Frame{}, errAudioProcessChanged
	}
	return decoded, nil
}
func (s *Service) endDispatchFrame(frame dispatch.Frame) {
	if s.crypto == nil {
		return
	}
	if owner := s.crypto.endReceive(frame); owner != nil {
		ctx, cancel := context.WithTimeout(s.operationCtx, 200*time.Millisecond)
		_ = owner.End(ctx, frame.CallID)
		cancel()
	}
}
func (s *Service) publishDispatchFrame(frame dispatch.Frame) {
	if s.hub == nil {
		return
	}
	if frame.End {
		s.endDispatchFrame(frame)
		frame.Encrypted = false
		frame.PCM = nil
		frame.Encoded = nil
		frame.P25 = nil
		s.hub.Publish(frame)
		return
	}
	if len(frame.Encoded) > 0 {
		decoded, err := s.decodeDispatchFrame(frame)
		if err != nil {
			message := "Radio audio processing is unavailable for this call."
			if frame.Encrypted {
				message = "Encrypted audio is unavailable for this call."
			}
			if errors.Is(err, dispatch.ErrP25SynchronizationPending) {
				message = "Waiting for encryption synchronization."
			}
			if errors.Is(err, errAudioProcessChanged) {
				message = "Audio processing changed; waiting for the next call."
			}
			s.hub.PublishUnavailable(frame, message)
			return
		}
		owner := s.crypto.activeReceiveOwner(frame)
		if owner == nil || !s.currentPacketCodec(owner) {
			if owner != nil {
				s.crypto.blockReceive(frame, owner)
			}
			s.hub.PublishUnavailable(frame, "Audio processing changed; waiting for the next call.")
			return
		}
		decoded.SourceAlias = frame.SourceAlias
		decoded.ReceivedEncrypted = frame.Encrypted
		decoded.ReceivedKeyID = int(frame.KeyID)
		decoded.ReceivedAlgorithmID = int(frame.AlgorithmID)
		s.hub.Publish(decoded)
		return
	}
	s.hub.Publish(frame)
}
