package framework

import (
	"errors"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25crypto"
	"open25.local/open25-fne/internal/routing"
	"slices"
	"time"
)

// Strapping belongs to the transmitting endpoint. Controllers admit the mode
// and key identity actually sent; they never turn received clear voice secure.
func (s *Service) prepareTransmit(q dispatch.CallRequest) (dispatch.CallRequest, error) {
	if q.EncryptionPending {
		return q, dispatch.ErrPermission
	}
	selected := false
	requested := q.Secure
	requestedAlgorithm, requestedKey := q.AlgorithmID, q.KeyID
	for _, id := range q.Talkgroups {
		index := slices.IndexFunc(s.view().System.Talkgroups, func(g Talkgroup) bool { return g.TGID == id && g.Enabled })
		if index < 0 {
			return q, dispatch.ErrPermission
		}
		g := s.view().System.Talkgroups[index]
		secure := requested
		switch g.TransmitMode {
		case "clear":
			secure = false
		case "secure":
			secure = true
		case "selectable":
		default:
			return q, errors.New("the talkgroup transmit mode cannot be used")
		}
		if selected && secure != q.Secure {
			return q, errors.New("transmit groups require the same clear or encrypted mode")
		}
		if secure {
			algorithm, key := requestedAlgorithm, requestedKey
			if algorithm == 0 {
				algorithm, key = g.AlgorithmID, g.KeyID
			}
			if !p25crypto.ValidSelection(true, algorithm, key) {
				return q, errors.New("the transmit key cannot be used")
			}
			if selected && (q.AlgorithmID != algorithm || q.KeyID != key) {
				return q, errors.New("encrypted transmit groups require a shared key")
			}
			q.AlgorithmID, q.KeyID = algorithm, key
		} else {
			q.AlgorithmID, q.KeyID = 0, 0
		}
		q.Secure = secure
		selected = true
	}
	return q, nil
}

func (s *Service) prepareCall(q dispatch.CallRequest, kind string, trustedZone bool) (dispatch.CallRequest, error) {
	if q.EncryptionPending && (kind != "rf" || !q.Secure || q.AlgorithmID != 0 || q.KeyID != 0) {
		return q, dispatch.ErrPermission
	}
	priority := 10
	groups := q.Talkgroups
	if len(q.AuthorizationGroups) > 0 && !(trustedZone && s.Config().Role == RoleSite) {
		groups = q.AuthorizationGroups
	}
	emergency := false
	for _, tg := range groups {
		if s.activeEmergency(q.RadioID, tg) {
			emergency = true
		}
	}
	q.Emergency = emergency || ((trustedZone || kind == "rf") && q.Emergency)
	for _, id := range groups {
		var found *Talkgroup
		for _, g := range s.view().System.Talkgroups {
			if g.TGID == id && g.Enabled {
				copy := g
				found = &copy
				break
			}
		}
		if found == nil {
			return q, dispatch.ErrPermission
		}
		if found.Priority >= 2 && found.Priority < priority {
			priority = found.Priority
		}
		switch found.AdmissionMode {
		case "clear":
			if q.Secure {
				return q, errors.New("this talkgroup accepts clear calls")
			}
		case "secure":
			if !q.Secure {
				return q, errors.New("this talkgroup requires an encrypted call")
			}
		case "both":
		default:
			return q, errors.New("the talkgroup call admission mode cannot be used")
		}
		if q.Secure && !q.EncryptionPending && !p25crypto.ValidSelection(true, q.AlgorithmID, q.KeyID) {
			return q, errors.New("the encrypted call identity cannot be used")
		}
	}
	if (kind == "application" || kind == "automation") && q.Secure && !(trustedZone && s.Config().Role == RoleSite) {
		if kind == "automation" {
			record, ok := s.view().Automations[q.AutomationID]
			if q.UserID != "" || !ok || !slices.Contains(record.Automation.Actions, "secure_ptt") {
				return q, dispatch.ErrPermission
			}
		} else {
			record, ok := s.view().Users[q.UserID]
			if q.AutomationID != "" || !ok || !slices.Contains(record.User.Permissions, "secure_ptt") {
				return q, dispatch.ErrPermission
			}
		}
	}
	q.Priority = priority
	if q.Emergency {
		q.Priority = 1
	}
	return q, nil
}
func (b *callBroker) callPolicy(q routing.Request) routing.Policy {
	mode := routing.FASTStart
	for _, tg := range q.Talkgroups {
		for _, g := range b.s.view().System.Talkgroups {
			if g.TGID == tg && g.StartPolicy == routing.AllStart {
				mode = routing.AllStart
			}
		}
	}
	return routing.Policy{StartMode: mode, Wait: 3 * time.Second, Retry: 100 * time.Millisecond}
}
func (b *callBroker) expandPatch(q routing.Request) (routing.Request, error) {
	if q.ZoneAuthorized && b.s.Config().Role == RoleSite {
		return q, nil
	}
	expanded, err := b.s.expandPatchedGroups(q.Talkgroups)
	if err != nil {
		return q, err
	}
	for _, p := range b.s.activePatches() {
		overlap := false
		for _, tg := range q.Talkgroups {
			if slices.Contains(p.Talkgroups, tg) {
				overlap = true
			}
		}
		if overlap && ((p.AlgorithmID != 0) != q.Secure || (q.Secure && !q.EncryptionPending && (p.KeyID != q.KeyID || p.AlgorithmID != q.AlgorithmID))) {
			return q, errors.New("this patch requires its shared encryption key and transmit mode")
		}
	}
	q.Talkgroups = expanded
	return q, nil
}

// Current locally generated voice uses the existing normal RF priority4.
// Talkgroup.Priority is the separate1..10 allocation policy, not this3-bit field.
func generatedVoiceService(emergency, protected bool) uint8 {
	value := uint8(4)
	if emergency {
		value |= 0x80
	}
	if protected {
		value |= 0x40
	}
	return value
}
