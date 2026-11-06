package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"open25.local/open25-fne/internal/dispatch"
	"slices"
	"strings"
	"time"
)

const audioIdentityLeaseDuration = 8 * time.Second

type audioIdentityLease struct {
	Owner, Session, UserID string
	Expires                time.Time
}
type audioIdentityInput struct {
	Actor   OperatorActor `json:"actor"`
	Session string        `json:"session"`
	Owner   string        `json:"owner,omitempty"`
}

type audioIdentityRoute struct {
	Actor     OperatorActor
	Addresses []string
}

func (s *Service) audioIdentityInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	role := s.Config().Role
	forwarded := role == RoleZone && peer.Role == RoleSite && s.view().Zone != nil && siteExists(s.view().Zone, peer.InstanceID)
	if (peer.Role != RoleApplication && !forwarded) || (role != RoleZone && role != RoleSite) {
		fail(w, 403, dispatch.ErrPermission)
		return
	}
	var in audioIdentityInput
	if err := readJSON(w, r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	owner := peer.InstanceID
	if forwarded {
		credentials, err := readCredentials(s.Config().CredentialFile)
		if err != nil || !slices.ContainsFunc(mapPeerValues(credentials.Peers), func(p PeerCredential) bool { return p.Role == RoleApplication && p.InstanceID == in.Owner }) {
			fail(w, 403, dispatch.ErrPermission)
			return
		}
		owner = in.Owner
	} else if in.Owner != "" {
		fail(w, 403, dispatch.ErrPermission)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/internal/audio-identity/")
	if !recordID.MatchString(owner) || (in.Actor.UserID == "") == (in.Actor.AutomationID == "") || in.Actor.RadioID < 1 || in.Session == "" || len(in.Session) > 100 {
		fail(w, 403, dispatch.ErrPermission)
		return
	}
	if action != "release" {
		if in.Actor.AutomationID != "" {
			if !s.automationCallAllowed(dispatch.CallRequest{AutomationID: in.Actor.AutomationID, AutomationAction: in.Actor.AutomationAction, RadioID: in.Actor.RadioID}, in.Actor.Proof, nil) {
				fail(w, 403, dispatch.ErrPermission)
				return
			}
		} else {
			authority, err := s.operatorActor(in.Actor)
			if err != nil || authority.admin {
				fail(w, 403, dispatch.ErrPermission)
				return
			}
		}
	}
	if err := s.updateAudioIdentity(owner, in.Session, actorIdentityKey(in.Actor), in.Actor.RadioID, action, time.Now()); err != nil {
		fail(w, 409, err)
		return
	}
	// A Site-connected console is a normal connection. While connected, Site
	// submits the same canonical Application Server owner to Zone; during isolation the
	// reservation is Site-local and is reconciled when Zone returns.
	if role == RoleSite && s.mediaClient != nil {
		if address := s.mediaClient.URL(); address != "" {
			in.Owner = owner
			upstreamAction := action
			if action == "renew" {
				upstreamAction = "claim"
			}
			if err := s.sendAudioIdentity(r.Context(), address, upstreamAction, in); err != nil {
				if !audioIdentityTransportError(err) {
					_ = s.updateAudioIdentity(owner, in.Session, actorIdentityKey(in.Actor), in.Actor.RadioID, "release", time.Now())
				}
				status := http.StatusConflict
				if audioIdentityTransportError(err) {
					status = http.StatusServiceUnavailable
				}
				fail(w, status, err)
				return
			}
		}
	}
	respond(w, 200, map[string]bool{"accepted": true})
}

func mapPeerValues(values map[string]PeerCredential) []PeerCredential {
	out := make([]PeerCredential, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func audioIdentityTransportError(err error) bool {
	var network *url.Error
	return errors.Is(err, dispatch.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.As(err, &network)
}

func (s *Service) sendAudioIdentity(ctx context.Context, address, action string, input audioIdentityInput) error {
	credentials, err := readCredentials(s.Config().CredentialFile)
	if err != nil {
		return err
	}
	if credentials.OutboundID == "" || len(credentials.OutboundToken) < 32 {
		return dispatch.ErrPermission
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(address, "/")+"/internal/audio-identity/"+action, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Service-ID", credentials.OutboundID)
	request.Header.Set("Authorization", "Bearer "+credentials.OutboundToken)
	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", dispatch.ErrUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil
	}
	var failure struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure)
	if failure.Error == "" {
		failure.Error = "audio identity request was refused"
	}
	if response.StatusCode >= 500 {
		return fmt.Errorf("%w: %s", dispatch.ErrUnavailable, failure.Error)
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusUnauthorized {
		return dispatch.ErrPermission
	}
	return errors.New(failure.Error)
}
func (s *Service) updateAudioIdentity(owner, session, user string, rid int, action string, now time.Time) error {
	s.audioLeaseMu.Lock()
	defer s.audioLeaseMu.Unlock()
	if s.audioLeases == nil {
		s.audioLeases = map[int]audioIdentityLease{}
	}
	for id, lease := range s.audioLeases {
		if !now.Before(lease.Expires) {
			delete(s.audioLeases, id)
		}
	}
	old, exists := s.audioLeases[rid]
	owned := exists && old.Owner == owner && old.Session == session && old.UserID == user
	switch action {
	case "claim":
		if exists && !owned {
			return errors.New("this radio ID already has an active audio session")
		}
	case "renew":
		if !owned {
			return errors.New("audio identity ownership has expired")
		}
	case "release":
		if owned {
			delete(s.audioLeases, rid)
		}
		return nil
	default:
		return errors.New("unknown audio identity action")
	}
	s.audioLeases[rid] = audioIdentityLease{owner, session, user, now.Add(audioIdentityLeaseDuration)}
	return nil
}
func (n *networkRouter) audioIdentity(ctx context.Context, p dispatch.Principal, id, action string) error {
	s := n.service
	if action == "release" {
		return n.releaseAudioIdentity(ctx, p, id)
	}
	if s.mediaClient == nil {
		return dispatch.ErrUnavailable
	}
	address, generation := s.mediaClient.Link()
	if address == "" {
		return dispatch.ErrUnavailable
	}
	actor, err := s.applicationAudioActor(p)
	if err != nil {
		return err
	}
	n.mu.Lock()
	old, exists := n.audioRoutes[id]
	n.mu.Unlock()
	if (exists && (actorIdentityKey(old.Actor) != actorIdentityKey(actor) || old.Actor.RadioID != p.RadioID)) || (action != "claim" && !exists) {
		return dispatch.ErrPermission
	}
	in := audioIdentityInput{Actor: actor, Session: id}
	// Keep attempted authorities too: a timed-out response does not prove that
	// the remote controller failed to accept the claim. Closing this admission
	// must be able to retire that reservation rather than wait for expiry.
	n.mu.Lock()
	if n.audioRoutes == nil {
		n.audioRoutes = map[string]audioIdentityRoute{}
	}
	route := n.audioRoutes[id]
	route.Actor = in.Actor
	if !slices.Contains(route.Addresses, address) {
		route.Addresses = append(route.Addresses, address)
	}
	n.audioRoutes[id] = route
	n.mu.Unlock()
	// Claim is idempotent for this owner/session. Renewing on a replacement
	// controller must establish ownership there before audio can use its route.
	if err := s.sendAudioIdentity(ctx, address, "claim", in); err != nil {
		if audioIdentityTransportError(err) {
			return fmt.Errorf("%w: %v", dispatch.ErrUnavailable, err)
		}
		return err
	}
	current, currentGeneration := s.mediaClient.Link()
	if current != address || generation != currentGeneration {
		return dispatch.ErrUnavailable
	}
	return s.updateAudioIdentity(s.Config().InstanceID, id, actorIdentityKey(actor), p.RadioID, "claim", time.Now())
}

func (n *networkRouter) releaseAudioIdentity(ctx context.Context, p dispatch.Principal, id string) error {
	n.mu.Lock()
	route, ok := n.audioRoutes[id]
	if ok && route.Actor.UserID == p.UserID && route.Actor.AutomationID == p.AutomationID && route.Actor.RadioID == p.RadioID {
		delete(n.audioRoutes, id)
	} else {
		ok = false
	}
	n.mu.Unlock()
	if !ok {
		return nil
	}
	_ = n.service.updateAudioIdentity(n.service.Config().InstanceID, id, actorIdentityKey(route.Actor), p.RadioID, "release", time.Now())
	var last error
	for i := len(route.Addresses) - 1; i >= 0; i-- {
		if err := n.service.sendAudioIdentity(ctx, route.Addresses[i], "release", audioIdentityInput{Actor: route.Actor, Session: id}); err != nil {
			last = err
		}
	}
	return last
}

func (n *networkRouter) ensureAudioIdentity(ctx context.Context, p dispatch.Principal, id string) error {
	return n.audioIdentity(ctx, p, id, "renew")
}
func (n *networkRouter) ClaimAudio(ctx context.Context, p dispatch.Principal, id string) error {
	return n.audioIdentity(ctx, p, id, "claim")
}
func (n *networkRouter) RenewAudio(ctx context.Context, p dispatch.Principal, id string) error {
	return n.audioIdentity(ctx, p, id, "renew")
}
func (n *networkRouter) ReleaseAudio(ctx context.Context, p dispatch.Principal, id string) error {
	return n.audioIdentity(ctx, p, id, "release")
}
