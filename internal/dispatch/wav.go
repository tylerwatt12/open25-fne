package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strconv"
	"time"
)

// ParseWAV accepts bounded uncompressed RIFF/WAVE. It never invokes a decoder,
// shell, downloaded codec or a user-supplied executable.
func ParseWAV(data []byte) ([]byte, error) {
	bad := errors.New("choose an 8 kHz, mono, 16-bit PCM WAV file, up to 30 seconds")
	if len(data) < 12 || len(data) > 512<<10 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 != uint64(len(data)) {
		return nil, bad
	}
	var pcm []byte
	format := false
	chunks := 0
	for at := 12; at < len(data); {
		chunks++
		if chunks > 128 || len(data)-at < 8 {
			return nil, bad
		}
		size := uint64(binary.LittleEndian.Uint32(data[at+4 : at+8]))
		start := uint64(at + 8)
		end := start + size
		next := end + size%2
		if end > uint64(len(data)) || next > uint64(len(data)) {
			return nil, bad
		}
		body := data[int(start):int(end)]
		switch string(data[at : at+4]) {
		case "fmt ":
			if format || (size != 16 && size != 18) {
				return nil, bad
			}
			if binary.LittleEndian.Uint16(body) != 1 || binary.LittleEndian.Uint16(body[2:]) != 1 || binary.LittleEndian.Uint32(body[4:]) != 8000 || binary.LittleEndian.Uint32(body[8:]) != 16000 || binary.LittleEndian.Uint16(body[12:]) != 2 || binary.LittleEndian.Uint16(body[14:]) != 16 || (size == 18 && binary.LittleEndian.Uint16(body[16:]) != 0) {
				return nil, bad
			}
			format = true
		case "data":
			if !format || pcm != nil || size == 0 || size > 8000*2*30 || size%2 != 0 {
				return nil, bad
			}
			pcm = append([]byte(nil), body...)
		}
		at = int(next)
	}
	if !format || len(pcm) == 0 {
		return nil, bad
	}
	return pcm, nil
}

func parseGroups(values []string) ([]int, error) {
	groups := []int{}
	for _, value := range values {
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, errors.New("talkgroup_id must be a decimal number")
		}
		groups = append(groups, n)
	}
	groups, err := groupList(groups)
	if err != nil || len(groups) == 0 {
		return nil, errors.New("select one or more permitted talkgroups")
	}
	return groups, nil
}

// PlayPCM feeds the same floor/lease/routing boundary as microphone PTT. A
// caller's existing listening connection may own the file job; it cannot start
// another PTT while that job owns its floor. Speech is paced, never replayed.
func (h *Hub) PlayPCM(ctx context.Context, p Principal, sessionKey string, groups []int, pcm []byte, secure bool, algorithmID, keyID int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > 8000*2*30 {
		return 0, errors.New("audio must be PCM16LE mono, up to 30 seconds")
	}
	var err error
	p, err = validPrincipal(p)
	if err != nil {
		return 0, err
	}
	digest := sha256.Sum256([]byte(sessionKey))
	h.mu.Lock()
	c := h.byRadio[p.RadioID]
	if c != nil && (c.principal.UserID != p.UserID || c.principal.AutomationID != p.AutomationID || c.principal.AutomationAction != p.AutomationAction || c.session != digest) {
		h.mu.Unlock()
		return 0, ErrPermission
	}
	h.mu.Unlock()
	created := false
	if c == nil {
		c, err = h.openClient(p, sessionKey)
		if err != nil {
			return 0, err
		}
		created = true
		defer h.removeClient(c)
	}
	if !c.file.CompareAndSwap(false, true) {
		return 0, errors.New("another file is already playing")
	}
	defer c.file.Store(false)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	id, err := randomID()
	if err != nil {
		return 0, err
	}
	tx, err := h.start(c, groups, id, true, secure, algorithmID, keyID)
	if err != nil {
		return 0, err
	}
	defer h.stop(tx, "")
	// Ephemeral file clients have no socket writer. Drain control notifications
	// so a longer file cannot fill the bounded state channel.
	drainStop := make(chan struct{})
	if created {
		defer close(drainStop)
		go func() {
			for {
				select {
				case <-drainStop:
					return
				case <-c.ctx.Done():
					return
				case <-c.send:
				}
			}
		}()
	}
	lease := time.NewTicker(200 * time.Millisecond)
	defer lease.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-tx.Done:
			return 0, h.transmissionError(tx)
		case <-lease.C:
			if err = h.renew(c, id); err != nil {
				if cause := h.awaitTransmissionError(ctx, tx); cause != nil {
					return 0, cause
				}
				return 0, err
			}
		case <-tx.Ready:
			goto ready
		}
	}
ready:
	h.mu.Lock()
	granted := tx.State == "transmitting"
	h.mu.Unlock()
	if !granted {
		if err := h.transmissionError(tx); err != nil {
			return 0, err
		}
		return 0, ErrUnavailable
	}
	pace := time.NewTicker(FrameDuration)
	defer pace.Stop()
	frames := 0
	total := (len(pcm) + FrameBytes - 1) / FrameBytes
	for {
		h.mu.Lock()
		written := tx.Written
		h.mu.Unlock()
		if frames == total && written >= uint64(total) {
			h.stop(tx, "")
			select {
			case <-tx.Done:
				return frames, h.transmissionError(tx)
			case <-ctx.Done():
				return frames, ctx.Err()
			}
		}
		select {
		case <-ctx.Done():
			return frames, ctx.Err()
		case <-tx.Done:
			return frames, h.transmissionError(tx)
		case <-lease.C:
			if err = h.renew(c, id); err != nil {
				if cause := h.awaitTransmissionError(ctx, tx); cause != nil {
					return frames, cause
				}
				return frames, err
			}
		case <-pace.C:
			if frames < total {
				frame := make([]byte, FrameBytes)
				begin := frames * FrameBytes
				end := min(len(pcm), begin+FrameBytes)
				copy(frame, pcm[begin:end])
				if err = h.acceptPCM(c, frame, time.Now()); err != nil {
					if cause := h.awaitTransmissionError(ctx, tx); cause != nil {
						return frames, cause
					}
					return frames, err
				}
				frames++
			}
		}
	}
}

// A producer can notice cancellation before the routing worker has completed
// its deferred release. Preserve that worker's actual failure, rather than
// replacing it with the secondary "no granted transmission" symptom.
func (h *Hub) awaitTransmissionError(ctx context.Context, tx *transmission) error {
	if tx.Context.Err() != nil {
		select {
		case <-tx.Done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h.transmissionError(tx)
}

func (h *Hub) transmissionError(tx *transmission) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if tx.Error != "" {
		return errors.New(tx.Error)
	}
	return nil
}
