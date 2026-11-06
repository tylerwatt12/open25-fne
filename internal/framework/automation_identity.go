package framework

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"open25.local/open25-fne/internal/dispatch"
	"slices"
)

// Automation is a machine identity, not a human login or an enrolled service.
// Its Radio ID must have the separate automation kind in the System directory.
type Automation struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	RadioID    int      `json:"radio_id"`
	Enabled    bool     `json:"enabled"`
	Talkgroups []int    `json:"talkgroups"`
	Actions    []string `json:"actions"`
}

type automationRecord struct {
	Automation Automation `json:"automation"`
	TokenHash  []byte     `json:"token_hash"`
}

func (s *Store) automationKey() string {
	if s.role == RoleSystem {
		return "automations"
	}
	return "cache:automations"
}

func (s *Store) automationRecords() ([]automationRecord, error) {
	var raw []byte
	err := s.db.QueryRow("SELECT document FROM framework_documents WHERE name=?", s.automationKey()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []automationRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []automationRecord
	if err = DecodeStrict(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []automationRecord{}
	}
	return out, nil
}

func (s *Store) Automations() ([]Automation, error) {
	records, err := s.automationRecords()
	if err != nil {
		return nil, err
	}
	out := []Automation{}
	for _, record := range records {
		out = append(out, record.Automation)
	}
	return out, nil
}

func canonicalAutomation(a Automation) Automation {
	a.Talkgroups = append([]int{}, a.Talkgroups...)
	a.Actions = append([]string{}, a.Actions...)
	slices.Sort(a.Talkgroups)
	slices.Sort(a.Actions)
	return a
}

func (s *Store) saveAutomationRecords(records []automationRecord) error {
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at", s.automationKey(), raw, nowStamp())
	return err
}

// The plaintext credential is returned only on creation or explicit rotation.
// Random 256-bit tokens use SHA-256 verifiers; they are not human passwords.
func (s *Store) SaveAutomation(id string, a Automation, create bool) (string, error) {
	if s.role != RoleSystem {
		return "", errors.New("automation accounts belong to System Administration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.automationRecords()
	if err != nil {
		return "", err
	}
	index := slices.IndexFunc(records, func(r automationRecord) bool { return r.Automation.ID == id })
	token := ""
	if index < 0 {
		record := automationRecord{Automation: a}
		if create {
			token, err = randomToken()
			if err != nil {
				return "", err
			}
			hash := sha256.Sum256([]byte(token))
			record.TokenHash = hash[:]
		}
		records = append(records, record)
	} else {
		records[index].Automation = a
	}
	if err = s.saveAutomationRecords(records); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) RotateAutomationToken(id string) (string, error) {
	if s.role != RoleSystem {
		return "", errors.New("automation credentials belong to System Administration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.automationRecords()
	if err != nil {
		return "", err
	}
	index := slices.IndexFunc(records, func(r automationRecord) bool { return r.Automation.ID == id })
	if index < 0 {
		return "", sql.ErrNoRows
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(token))
	records[index].TokenHash = hash[:]
	if err = s.saveAutomationRecords(records); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) DeleteAutomation(id string) error {
	if s.role != RoleSystem {
		return errors.New("automation accounts belong to System Administration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.automationRecords()
	if err != nil {
		return err
	}
	return s.saveAutomationRecords(slices.DeleteFunc(records, func(r automationRecord) bool { return r.Automation.ID == id }))
}

func eligibleAutomations(records []automationRecord, system SystemConfig) []automationRecord {
	out := []automationRecord{}
	for _, record := range records {
		a := canonicalAutomation(record.Automation)
		if !a.Enabled || !slices.ContainsFunc(system.Radios, func(r Radio) bool { return r.RID == a.RadioID && r.Enabled && r.Kind == "automation" }) {
			continue
		}
		a.Talkgroups = slices.DeleteFunc(a.Talkgroups, func(tg int) bool {
			return !slices.ContainsFunc(system.Talkgroups, func(g Talkgroup) bool { return g.TGID == tg && g.Enabled })
		})
		if len(a.Talkgroups) == 0 {
			continue
		}
		record.Automation = a
		record.TokenHash = slices.Clone(record.TokenHash)
		out = append(out, record)
	}
	return out
}

func automationProof(record automationRecord, action string) string {
	raw, _ := json.Marshal(struct {
		ID        string
		RadioID   int
		TokenHash []byte
		Action    string
	}{record.Automation.ID, record.Automation.RadioID, record.TokenHash, action})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func (s *Service) automationActor(id, action string, rid int) (OperatorActor, error) {
	record, ok := s.view().Automations[id]
	if !ok || !record.Automation.Enabled || record.Automation.RadioID != rid || (action != "wav" && action != "tts") || !slices.Contains(record.Automation.Actions, action) {
		return OperatorActor{}, dispatch.ErrPermission
	}
	return OperatorActor{AutomationID: id, AutomationAction: action, RadioID: rid, Proof: automationProof(record, action)}, nil
}

func (s *Service) automationCallAllowed(q dispatch.CallRequest, proof string, groups []int) bool {
	if q.UserID != "" || q.AutomationID == "" {
		return false
	}
	actor, err := s.automationActor(q.AutomationID, q.AutomationAction, q.RadioID)
	if err != nil || subtle.ConstantTimeCompare([]byte(proof), []byte(actor.Proof)) != 1 {
		return false
	}
	a := s.view().Automations[q.AutomationID].Automation
	return (!q.Secure || slices.Contains(a.Actions, "secure_ptt")) && !slices.ContainsFunc(groups, func(tg int) bool { return !slices.Contains(a.Talkgroups, tg) })
}

func actorIdentityKey(actor OperatorActor) string {
	if actor.AutomationID != "" {
		return "automation:" + actor.AutomationID
	}
	return "user:" + actor.UserID
}

func (s *Service) applicationAudioActor(p dispatch.Principal) (OperatorActor, error) {
	if p.AutomationID != "" {
		if p.UserID != "" || len(p.ReceiveGroups) != 0 {
			return OperatorActor{}, dispatch.ErrPermission
		}
		actor, err := s.automationActor(p.AutomationID, p.AutomationAction, p.RadioID)
		if err != nil {
			return actor, err
		}
		a := s.view().Automations[p.AutomationID].Automation
		if !p.CanTransmit || (p.CanSecure && !slices.Contains(a.Actions, "secure_ptt")) || slices.ContainsFunc(p.TransmitGroups, func(tg int) bool { return !slices.Contains(a.Talkgroups, tg) }) {
			return OperatorActor{}, dispatch.ErrPermission
		}
		return actor, nil
	}
	if p.UserID == "" || p.AutomationAction != "" {
		return OperatorActor{}, dispatch.ErrPermission
	}
	record, ok := s.view().Users[p.UserID]
	if !ok {
		return OperatorActor{}, dispatch.ErrPermission
	}
	effective, _, err := resolveUserRadio(record.User, s.view().System, p.RadioID)
	if err != nil || !effective.Enabled || (p.CanTransmit && !slices.Contains(effective.Permissions, "ptt")) || (p.CanSecure && !slices.Contains(effective.Permissions, "secure_ptt")) || slices.ContainsFunc(p.ReceiveGroups, func(tg int) bool { return !slices.Contains(effective.Talkgroups, tg) }) || slices.ContainsFunc(p.TransmitGroups, func(tg int) bool { return !slices.Contains(effective.Talkgroups, tg) }) {
		return OperatorActor{}, dispatch.ErrPermission
	}
	return OperatorActor{UserID: p.UserID, RadioID: p.RadioID, Proof: userProof(record.User, record.PasswordHash)}, nil
}
