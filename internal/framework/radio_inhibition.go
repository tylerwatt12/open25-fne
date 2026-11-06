package framework

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// The Zone owns current desired inhibition. Omission means desired false;
// actual blocked/pending state belongs to the Site's current radio owner.
type radioInhibitionPolicy struct {
	Desired []int `json:"desired"`
}

// A complete transport/cache document always contains an array, including []
// for authoritative clear. Missing/null is not a complete current policy.
func (p *radioInhibitionPolicy) UnmarshalJSON(raw []byte) error {
	var input struct {
		Desired *[]int `json:"desired"`
	}
	if err := DecodeStrict(raw, &input); err != nil {
		return err
	}
	if input.Desired == nil {
		return errors.New("inhibit policy requires its complete desired Radio ID array")
	}
	p.Desired = slices.Clone(*input.Desired)
	return nil
}

const maximumDesiredInhibitedRadios = 32

type radioInhibitionSnapshot struct {
	desired []int
	lookup  map[int]struct{}
	err     error
}

func normalizedRadioInhibition(value radioInhibitionPolicy) (radioInhibitionPolicy, error) {
	if len(value.Desired) > maximumDesiredInhibitedRadios {
		return radioInhibitionPolicy{}, errors.New("the desired inhibit set exceeds the supported 32-radio limit")
	}
	desired := slices.Clone(value.Desired)
	if desired == nil {
		desired = []int{}
	}
	slices.Sort(desired)
	for i, rid := range desired {
		if rid < 1 || rid > 0xFFFFFB {
			return radioInhibitionPolicy{}, errors.New("inhibit policy contains a reserved or invalid Radio ID")
		}
		if i > 0 && desired[i-1] == rid {
			return radioInhibitionPolicy{}, errors.New("inhibit policy contains a duplicate Radio ID")
		}
	}
	return radioInhibitionPolicy{Desired: desired}, nil
}
func inhibitionSnapshot(value radioInhibitionPolicy, err error) *radioInhibitionSnapshot {
	next := &radioInhibitionSnapshot{desired: slices.Clone(value.Desired), lookup: make(map[int]struct{}, len(value.Desired)), err: err}
	for _, rid := range next.desired {
		next.lookup[rid] = struct{}{}
	}
	return next
}
func (s *Service) currentRadioInhibition() *radioInhibitionSnapshot {
	if current := s.inhibition.Load(); current != nil {
		return current
	}
	s.inhibitionMu.Lock()
	defer s.inhibitionMu.Unlock()
	if current := s.inhibition.Load(); current != nil {
		return current
	}
	value := radioInhibitionPolicy{Desired: []int{}}
	var err error
	if s.Store == nil {
		err = errors.New("radio inhibit policy store is unavailable")
	} else {
		var raw json.RawMessage
		raw, _, err = s.Store.ReadCache("radio-inhibition")
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		} else if err == nil {
			err = DecodeStrict(raw, &value)
			if err == nil {
				value, err = normalizedRadioInhibition(value)
			}
		}
	}
	current := inhibitionSnapshot(value, err)
	s.inhibition.Store(current)
	return current
}

// Repeated admission reads use an immutable snapshot, without database access.
func (s *Service) DesiredInhibitedRadios() ([]int, error) {
	current := s.currentRadioInhibition()
	if current.err != nil {
		return nil, current.err
	}
	return slices.Clone(current.desired), nil
}
func (s *Service) desiredRadioInhibited(rid int) (bool, error) {
	current := s.currentRadioInhibition()
	if current.err != nil {
		return false, current.err
	}
	_, blocked := current.lookup[rid]
	return blocked, nil
}

// Caller establishes Zone authority and serializes full replacement with
// syncMu (Site) or commandMu (Zone). Persistence precedes publication.
func (s *Service) cacheRadioInhibition(value radioInhibitionPolicy) error {
	role := s.Config().Role
	if role != RoleZone && role != RoleSite {
		return errors.New("radio inhibit policy belongs to the Zone and its enrolled Sites")
	}
	value, err := normalizedRadioInhibition(value)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.inhibitionMu.Lock()
	defer s.inhibitionMu.Unlock()
	if s.Store == nil {
		return errors.New("radio inhibit policy store is unavailable")
	}
	if err = s.Store.WriteCache("radio-inhibition", raw); err != nil {
		return err
	}
	s.inhibition.Store(inhibitionSnapshot(value, nil))
	return nil
}

func inhibitionCommand(action string) bool { return action == "inhibit" || action == "uninhibit" }

// commandMu covers admission and the selected Site roundtrip. Catalogue reads
// only load the immutable snapshot, so a Site synchronizing while holding its
// syncMu cannot deadlock this command's HTTP handler.
func (s *Service) sendZoneRadioInhibition(ctx context.Context, input radioCommandInput, actor OperatorActor, endpoint string, peer PeerCredential) (radioCommandReceipt, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if input.ID == "" {
		id, err := operatorID("command")
		if err != nil {
			return radioCommandReceipt{}, err
		}
		input.ID = id
	}
	if !recordID.MatchString(input.ID) {
		return radioCommandReceipt{}, errors.New("invalid command ID")
	}
	receipt, duplicate, err := s.admitZoneRadioInhibition(input, actor)
	if err != nil {
		return receipt, err
	}
	if duplicate {
		receipt.Message = "Existing command receipt; current Zone desired policy is unchanged. " + receipt.Message
		if !receipt.Sent {
			return receipt, errors.New(receipt.Message)
		}
		return receipt, nil
	}
	desired, err := s.DesiredInhibitedRadios()
	if err != nil {
		return receipt, err
	}
	var site radioCommandReceipt
	err = s.requestAt(ctx, endpoint, "POST", "/internal/radio/command", radioCommandEnvelope{Input: input, Actor: actor, Inhibition: &radioInhibitionPolicy{Desired: desired}}, &site, &peer)
	if err == nil && (site.ID != input.ID || !site.Sent || site.SiteID != input.SiteID || site.RadioID != input.RadioID || site.Action != input.Action) {
		err = errors.New("Site did not confirm the addressed radio command")
	}
	sendErr := err
	updated, err := s.updateZoneInhibitionReceipt(input.ID, func(v *radioCommandReceipt) {
		if sendErr != nil {
			v.State = "unconfirmed"
			v.Sent = false
			v.AwaitingAcknowledgment = false
			v.Message = "Zone desired inhibit policy saved; selected Site did not confirm sending the command: " + sendErr.Error()
			return
		}
		v.State = site.State
		v.Sent = site.Sent
		v.AwaitingAcknowledgment = site.AwaitingAcknowledgment
		v.AttemptedAt = site.AttemptedAt
		v.SentAt = site.SentAt
		v.ResponseAt = site.ResponseAt
		v.ResponseKind = site.ResponseKind
		v.ResponseEventID = site.ResponseEventID
		v.Message = site.Message
	})
	if err != nil {
		receipt.State = "unconfirmed"
		receipt.Message = "Zone desired inhibit policy saved; the selected Site command result could not be recorded: " + err.Error()
		return receipt, errors.New(receipt.Message)
	}
	receipt = updated
	if sendErr != nil {
		return receipt, errors.New(receipt.Message)
	}
	return receipt, nil
}

// Current intent and the existing request-ID receipt commit together. Retrying
// an old ID after the opposite choice therefore cannot reapply old intent.
func (s *Service) admitZoneRadioInhibition(input radioCommandInput, actor OperatorActor) (radioCommandReceipt, bool, error) {
	if s.Config().Role != RoleZone || !actor.Administrator || actor.UserID != "" || !inhibitionCommand(input.Action) {
		return radioCommandReceipt{}, false, errors.New("only a Zone administrator may choose inhibit policy")
	}
	current := s.currentRadioInhibition()
	if current.err != nil {
		return radioCommandReceipt{}, false, current.err
	}
	s.inhibitionMu.Lock()
	defer s.inhibitionMu.Unlock()
	current = s.inhibition.Load()
	if current.err != nil {
		return radioCommandReceipt{}, false, current.err
	}
	s.Store.mu.Lock()
	defer s.Store.mu.Unlock()
	rows, err := s.Store.radioCommandsLocked()
	if err != nil {
		return radioCommandReceipt{}, false, err
	}
	for _, old := range rows {
		if old.ID == input.ID {
			if old.Action != input.Action || old.RadioID != input.RadioID || old.SiteID != input.SiteID || old.ActorUserID != actor.UserID || old.ActorUsername != actor.Username || old.SourceRadioID != actor.RadioID {
				return radioCommandReceipt{}, false, errors.New("this command ID was already used for another request")
			}
			return old, true, nil
		}
	}
	desired := slices.Clone(current.desired)
	desired = slices.DeleteFunc(desired, func(rid int) bool { return rid == input.RadioID })
	if input.Action == "inhibit" {
		desired = append(desired, input.RadioID)
	}
	policy, err := normalizedRadioInhibition(radioInhibitionPolicy{Desired: desired})
	if err != nil {
		return radioCommandReceipt{}, false, err
	}
	now := time.Now().UTC()
	expireRadioCommands(rows, now)
	rows = slices.DeleteFunc(rows, func(v radioCommandReceipt) bool { return now.Sub(v.CreatedAt) > 7*24*time.Hour && !commandPending(v) })
	if len(rows) >= 1000 {
		i := slices.IndexFunc(rows, func(v radioCommandReceipt) bool { return !commandPending(v) })
		if i < 0 {
			return radioCommandReceipt{}, false, errors.New("radio command queue is full")
		}
		rows = append(rows[:i], rows[i+1:]...)
	}
	receipt := radioCommandReceipt{ID: input.ID, State: "accepted", SiteID: input.SiteID, RadioID: input.RadioID, Action: input.Action, ActorUsername: actor.Username, SourceRadioID: actor.RadioID, CreatedAt: now, ExpiresAt: now.Add(radioCommandWait), Message: "Zone desired inhibit policy saved; selected Site command is not yet confirmed."}
	rows = append(rows, receipt)
	policyRaw, err := json.Marshal(policy)
	if err != nil {
		return radioCommandReceipt{}, false, err
	}
	rowsRaw, err := json.Marshal(rows)
	if err != nil {
		return radioCommandReceipt{}, false, err
	}
	tx, err := s.Store.db.Begin()
	if err != nil {
		return radioCommandReceipt{}, false, err
	}
	defer tx.Rollback()
	for name, raw := range map[string][]byte{"cache:radio-inhibition": policyRaw, "radio-commands": rowsRaw} {
		if _, err = tx.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at", name, raw, nowStamp()); err != nil {
			return radioCommandReceipt{}, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return radioCommandReceipt{}, false, err
	}
	s.inhibition.Store(inhibitionSnapshot(policy, nil))
	return receipt, false, nil
}

// Site outcomes remain Site-owned. The Zone's dedupe copy is private command
// state and must not emit duplicate Site command event IDs upstream.
func (s *Service) updateZoneInhibitionReceipt(id string, update func(*radioCommandReceipt)) (radioCommandReceipt, error) {
	var receipt radioCommandReceipt
	err := s.Store.mutateRadioCommands(func(rows *[]radioCommandReceipt) error {
		for i := range *rows {
			if (*rows)[i].ID == id {
				update(&(*rows)[i])
				receipt = (*rows)[i]
				return nil
			}
		}
		return errors.New("Zone inhibit request receipt is unavailable")
	})
	return receipt, err
}
