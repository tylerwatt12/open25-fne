package framework

import (
	"errors"
	"strings"
	"unicode"
)

// The enrolled Zone owns authorization of its administrator's radio commands.
// Its authenticated assertion is scoped to this Site command endpoint; a local
// Site password verifier is neither the Zone identity nor a delegation token.
// Application users still require their current directory proof and permissions.
func (s *Service) radioCommandActor(peer PeerCredential, actor OperatorActor) (operatorAuthority, error) {
	zone := s.view().Zone
	if s.Config().Role != RoleSite || peer.Role != RoleZone || zone == nil || peer.InstanceID != zone.ID {
		return operatorAuthority{}, errors.New("this Zone is not authorized to command this Site")
	}
	if !actor.Administrator {
		return s.operatorActor(actor)
	}
	if actor.UserID != "" || actor.RadioID != 0 || actor.AutomationID != "" || actor.AutomationAction != "" ||
		actor.Username != strings.TrimSpace(actor.Username) || len(actor.Username) < 1 || len(actor.Username) > 64 ||
		strings.ContainsRune(actor.Username, ' ') || strings.IndexFunc(actor.Username, unicode.IsControl) >= 0 {
		return operatorAuthority{}, errors.New("invalid Zone administrator assertion")
	}
	// Proof belongs to application identities. It has no authority in this
	// enrolled administrator assertion and must not enter audit records.
	actor.Proof = ""
	return operatorAuthority{actor: actor, admin: true}, nil
}
