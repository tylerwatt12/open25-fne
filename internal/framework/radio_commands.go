package framework

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"open25.local/open25-fne/internal/radio"
)

type radioCommandInput struct {
	ID      string `json:"id,omitempty"`
	Action  string `json:"action"`
	RadioID int    `json:"radio_id"`
	SiteID  string `json:"site_id,omitempty"`
	Code    *int   `json:"code,omitempty"`
}

func supportedRadioCommand(action string) bool {
	switch action {
	case "radio_check", "page", "message", "registration_demand", "affiliation_query", "status_query", "inhibit", "uninhibit":
		return true
	default:
		return false
	}
}

func (s *Service) validateRadioCommand(input radioCommandInput) error {
	if s.Config().Role == RoleSite && input.SiteID != "" && input.SiteID != s.Config().InstanceID {
		return errors.New("this command is addressed to another Site")
	}
	if !supportedRadioCommand(input.Action) {
		return errors.New("this radio command is not implemented")
	}
	if input.Action == "message" && (input.Code == nil || *input.Code < 0 || *input.Code > 65535) {
		return errors.New("enter a message code from 0 to 65535")
	}
	if input.RadioID < 1 || input.RadioID > 0xFFFFFB {
		return errors.New("enter an unreserved Radio ID")
	}
	for _, known := range s.view().System.Radios {
		if known.RID == input.RadioID && known.Kind == "subscriber" {
			return nil
		}
	}
	return errors.New("select a subscriber radio from the system directory")
}

func (s *Service) radioCommandHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	role := s.Config().Role
	if r.URL.Path == "/api/radio/commands" {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		if session.UserID != "" || (role != RoleSite && role != RoleZone) {
			fail(w, 403, errors.New("administrator sign-in is required"))
			return
		}
		rows, err := s.commandRows(r.Context())
		if err != nil {
			fail(w, 503, err)
			return
		}
		respond(w, 200, rows)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	if session.UserID != "" || (role != RoleSite && role != RoleZone) {
		fail(w, http.StatusForbidden, errors.New("radio commands require Zone or Site administrator sign-in"))
		return
	}
	var input radioCommandInput
	if err := readJSON(w, r, &input); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.validateRadioCommand(input); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if role == RoleSite && inhibitionCommand(input.Action) {
		fail(w, http.StatusForbidden, errors.New("new inhibit and re-enable choices belong to the Zone administrator"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	username, _, err := s.Store.Account()
	if err != nil {
		fail(w, 503, err)
		return
	}
	actor := OperatorActor{Administrator: true, Username: username}
	if role == RoleSite {
		receipt, err := s.sendSiteRadioCommand(ctx, input, actor, 0)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, err)
			return
		}
		respond(w, http.StatusAccepted, receipt)
		return
	}
	if input.SiteID == "" {
		fail(w, http.StatusBadRequest, errors.New("select the Site that should send this command"))
		return
	}
	zone := s.view().Zone
	if zone == nil {
		fail(w, http.StatusServiceUnavailable, errors.New("Zone settings are not available"))
		return
	}
	endpoint := ""
	for _, site := range zone.Sites {
		if site.ID == input.SiteID {
			endpoint = site.Endpoint
			break
		}
	}
	if endpoint == "" {
		fail(w, http.StatusBadRequest, errors.New("select an enrolled Site with a configured server address"))
		return
	}
	_, peer, err := s.peerIdentityForSite(input.SiteID)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	if inhibitionCommand(input.Action) {
		receipt, policyErr := s.sendZoneRadioInhibition(ctx, input, actor, endpoint, peer)
		if policyErr != nil {
			if receipt.ID != "" {
				respond(w, http.StatusServiceUnavailable, map[string]any{"error": policyErr.Error(), "receipt": receipt, "desired_policy_saved": true})
			} else {
				fail(w, http.StatusServiceUnavailable, policyErr)
			}
			return
		}
		respond(w, http.StatusAccepted, receipt)
		return
	}
	var receipt radioCommandReceipt
	if err = s.requestAt(ctx, endpoint, "POST", "/internal/radio/command", radioCommandEnvelope{Input: input, Actor: actor}, &receipt, &peer); err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	if !receipt.Sent || receipt.SiteID != input.SiteID || receipt.RadioID != input.RadioID || receipt.Action != input.Action || !sameCommandCode(receipt.Code, input.Code) {
		fail(w, http.StatusBadGateway, errors.New("Site did not confirm that this command was sent"))
		return
	}
	respond(w, http.StatusAccepted, receipt)
}

func (s *Service) radioCommandInternal(w http.ResponseWriter, r *http.Request, peer PeerCredential) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if r.Context().Err() != nil {
		fail(w, 503, r.Context().Err())
		return
	}
	zone := s.view().Zone
	if s.Config().Role != RoleSite || peer.Role != RoleZone || zone == nil || peer.InstanceID != zone.ID {
		fail(w, http.StatusForbidden, errors.New("this Zone is not authorized to command this Site"))
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var envelope radioCommandEnvelope
	if err := readJSON(w, r, &envelope); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	input := envelope.Input
	authority, err := s.radioCommandActor(peer, envelope.Actor)
	if err != nil {
		fail(w, 403, err)
		return
	}
	if !authority.admin && (!operatorPermitted(authority, input.Action) || (input.Action != "radio_check" && input.Action != "page" && input.Action != "message") || !s.operatorGroupAllowed(authority, envelope.TalkgroupID)) {
		fail(w, 403, errors.New("this radio command is not permitted"))
		return
	}
	if !authority.admin {
		current := s.radioStatus()
		if !slices.ContainsFunc(current.Affiliations, func(a radio.Affiliation) bool { return a.RID == input.RadioID && a.TGID == envelope.TalkgroupID }) {
			fail(w, 403, errors.New("the target radio no longer has the expected affiliation at this Site"))
			return
		}
	}
	if err := s.validateRadioCommand(input); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if inhibitionCommand(input.Action) {
		if input.ID == "" || !recordID.MatchString(input.ID) {
			fail(w, 400, errors.New("inhibit command requires its Zone request ID"))
			return
		}
		if !authority.admin || envelope.Inhibition == nil {
			fail(w, 403, errors.New("inhibit commands require the Zone's complete authoritative policy"))
			return
		}
		policy, policyErr := normalizedRadioInhibition(*envelope.Inhibition)
		if policyErr != nil {
			fail(w, 400, policyErr)
			return
		}
		if slices.Contains(policy.Desired, input.RadioID) != (input.Action == "inhibit") {
			fail(w, 400, errors.New("radio command disagrees with Zone desired inhibit policy"))
			return
		}
		// A delayed retransmission of an already admitted ID must not restore
		// its old full-set envelope after a newer Zone choice.
		rows, readErr := s.Store.radioCommands()
		if readErr != nil {
			fail(w, 503, readErr)
			return
		}
		for _, old := range rows {
			if old.ID == input.ID {
				if old.Action != input.Action || old.RadioID != input.RadioID || old.ActorUserID != envelope.Actor.UserID || old.ActorUsername != envelope.Actor.Username || old.SourceRadioID != envelope.Actor.RadioID || old.TalkgroupID != envelope.TalkgroupID {
					fail(w, 400, errors.New("this command ID was already used for another request"))
					return
				}
				respond(w, http.StatusAccepted, old)
				return
			}
		}
		if ctx.Err() != nil {
			fail(w, 503, ctx.Err())
			return
		}
		if policyErr = s.cacheRadioInhibition(policy); policyErr != nil {
			fail(w, 503, policyErr)
			return
		}
		if policyErr = s.applySiteInhibitionPolicy(ctx, policy.Desired); policyErr != nil {
			fail(w, 503, policyErr)
			return
		}
	} else if envelope.Inhibition != nil {
		fail(w, 400, errors.New("this command cannot replace inhibit policy"))
		return
	}
	receipt, err := s.sendSiteRadioCommand(ctx, input, envelope.Actor, envelope.TalkgroupID)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	respond(w, http.StatusAccepted, receipt)
}

func (s *Service) sendSiteRadioCommand(ctx context.Context, input radioCommandInput, actor OperatorActor, group int) (radioCommandReceipt, error) {
	// Serialize request admission, not callbacks from the native adapter.
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	siteID := s.Config().InstanceID
	if input.SiteID != "" && input.SiteID != siteID {
		return radioCommandReceipt{}, errors.New("this command is addressed to another Site")
	}
	if input.ID == "" {
		var err error
		input.ID, err = operatorID("command")
		if err != nil {
			return radioCommandReceipt{}, err
		}
	}
	if !recordID.MatchString(input.ID) {
		return radioCommandReceipt{}, errors.New("invalid command ID")
	}
	now := time.Now().UTC()
	operation := radioCommandReceipt{ID: input.ID, State: "accepted", SiteID: siteID, RadioID: input.RadioID, TalkgroupID: group, Action: input.Action, Code: input.Code, ActorUserID: actor.UserID, ActorUsername: actor.Username, SourceRadioID: actor.RadioID, CreatedAt: now, ExpiresAt: now.Add(radioCommandWait), Message: "Radio command accepted; not yet sent."}
	if record, ok := s.view().Users[actor.UserID]; ok {
		_, position, _ := resolveUserRadio(record.User, s.view().System, actor.RadioID)
		if position != nil {
			operation.PositionID = position.ID
		}
	}
	duplicate := false
	err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		for _, old := range *rows {
			if old.ID == input.ID {
				if old.Action != operation.Action || old.RadioID != operation.RadioID || old.TalkgroupID != operation.TalkgroupID || old.ActorUserID != operation.ActorUserID || old.ActorUsername != operation.ActorUsername || old.SourceRadioID != operation.SourceRadioID || !sameCommandCode(old.Code, operation.Code) {
					return errors.New("this command ID was already used for another request")
				}
				operation = old
				duplicate = true
				return nil
			}
			if old.Action == operation.Action && old.RadioID == operation.RadioID && now.Before(old.ExpiresAt) {
				return errors.New("a request for this radio and action is still within its response window")
			}
		}
		kept := slices.DeleteFunc(*rows, func(v radioCommandReceipt) bool { return now.Sub(v.CreatedAt) > 7*24*time.Hour && !commandPending(v) })
		if len(kept) >= 1000 {
			i := slices.IndexFunc(kept, func(v radioCommandReceipt) bool { return !commandPending(v) })
			if i < 0 {
				return errors.New("radio command queue is full")
			}
			kept = append(kept[:i], kept[i+1:]...)
		}
		*rows = append(kept, operation)
		return nil
	})
	if err != nil {
		return radioCommandReceipt{}, err
	}
	if duplicate {
		return operation, nil
	}
	s.commandEvent(operation)
	if (input.Action == "page" || input.Action == "message") && s.radioServiceInhibited(input.RadioID) {
		result, updateErr := s.commandUpdated(input.ID, func(v *radioCommandReceipt) {
			v.State = "rejected"
			v.Message = "This radio remains inhibited at this Site; the requested service is unavailable."
		})
		if updateErr != nil {
			return result, updateErr
		}
		return result, errors.New(result.Message)
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if s.radio == nil || s.radio.Status().State != "running" {
		result, _ := s.commandUpdated(input.ID, func(v *radioCommandReceipt) {
			v.State = "rejected"
			v.Message = "Start this Site's control channel before sending radio commands."
		})
		return result, errors.New(result.Message)
	}
	if actor.UserID != "" && (!slices.ContainsFunc(s.radio.Status().Affiliations, func(a radio.Affiliation) bool { return a.RID == input.RadioID && a.TGID == group }) ||
		!slices.ContainsFunc(s.view().System.Radios, func(r Radio) bool { return r.RID == input.RadioID && r.Enabled && r.Kind == "subscriber" }) ||
		!groupAtSite(s.view().System, group, siteID)) {
		result, updateErr := s.commandUpdated(input.ID, func(v *radioCommandReceipt) {
			v.State = "rejected"
			v.Message = "The target radio no longer has the expected affiliation at this Site."
		})
		if updateErr != nil {
			return result, updateErr
		}
		return result, errors.New(result.Message)
	}
	var admissionErr error
	operation, err = s.commandUpdated(input.ID, func(v *radioCommandReceipt) {
		// Ownership can be busy after initial admission. Decide whether an
		// attempt is still allowed in the same transaction that records it;
		// the expiry worker must not race a stale accepted copy into a send.
		if v.State != "accepted" {
			admissionErr = errors.New("this radio command is no longer awaiting send")
			return
		}
		now := time.Now().UTC()
		if admissionErr = ctx.Err(); admissionErr == nil && !now.Before(v.ExpiresAt) {
			admissionErr = errors.New("radio command expired before it could be sent")
		}
		if admissionErr != nil {
			v.State = "rejected"
			v.AwaitingAcknowledgment = false
			v.Message = "Radio command not sent: " + admissionErr.Error()
			return
		}
		v.AttemptedAt = now
		if v.Action == "message" {
			// Native admission may finish after the HTTP caller leaves. Keep
			// this command's response fence beyond its 750 ms admission and
			// ten-second native reply window, measured from the actual attempt.
			v.ExpiresAt = now.Add(radioCommandWait)
		}
	})
	if err != nil {
		return operation, err
	}
	if admissionErr != nil {
		return operation, admissionErr
	}
	command := radio.Command{Action: input.Action, RID: input.RadioID}
	if input.Code != nil {
		command.Code = *input.Code
	}
	_, sendErr := s.radio.Command(ctx, command)
	operation, err = s.commandUpdated(input.ID, func(v *radioCommandReceipt) {
		// A native observation or the expiry worker may finish the request
		// before its initiating HTTP call returns. Transport completion can
		// add send evidence but cannot reopen a terminal command outcome.
		if !commandPending(*v) {
			if sendErr == nil || v.State == "acknowledged" || v.State == "response_observed" {
				v.Sent = true
				if v.SentAt.IsZero() {
					v.SentAt = v.AttemptedAt
				}
			}
			return
		}
		if sendErr != nil {
			v.State = "unconfirmed"
			v.AwaitingAcknowledgment = false
			v.Message = sendErr.Error()
			return
		}
		v.State = "sent"
		v.Sent = true
		v.AwaitingAcknowledgment = true
		v.SentAt = time.Now().UTC()
		v.Message = "Command sent; awaiting a matching subscriber response."
	})
	if err != nil {
		return operation, err
	}
	return operation, sendErr
}
