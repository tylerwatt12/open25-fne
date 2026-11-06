package framework

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// Audio admission/validation uses this immutable accepted policy. UI snapshots
// also contain live delivery information and read durable state; neither those
// locks nor configuration storage belong in a per-frame media operation.
type operatorPolicySnapshot struct {
	emergencies map[[2]int]bool
	patches     []RuntimePatch
}

// The caller holds featureMu. Publish only after accepted durable writes, so
// an unsuccessful change cannot become live call policy.
func (s *Service) publishOperatorPolicyLocked() error {
	read := func(name string) (OperatorSnapshot, error) {
		value := emptyOperatorSnapshot()
		raw, _, err := s.Store.ReadCache(name)
		if errors.Is(err, sql.ErrNoRows) {
			return value, nil
		}
		if err != nil {
			return value, err
		}
		err = json.Unmarshal(raw, &value)
		return value, err
	}
	local, err := read("operator-local")
	if err != nil {
		return err
	}
	upstream, err := read("operator-upstream")
	if err != nil {
		return err
	}
	value := mergeOperatorSnapshots(upstream, local)
	next := &operatorPolicySnapshot{emergencies: map[[2]int]bool{}, patches: make([]RuntimePatch, 0, len(value.Patches))}
	for _, emergency := range value.Emergencies {
		if emergency.ClearedAt.IsZero() {
			next.emergencies[[2]int{emergency.RadioID, emergency.TalkgroupID}] = true
		}
	}
	for _, patch := range value.Patches {
		patch.Talkgroups = slices.Clone(patch.Talkgroups)
		next.patches = append(next.patches, patch)
	}
	s.operatorPolicy.Store(next)
	return nil
}

func (s *Service) refreshOperatorPolicy() {
	s.featureMu.Lock()
	err := s.publishOperatorPolicyLocked()
	s.featureMu.Unlock()
	if err != nil {
		s.runtimeError(errors.New("could not refresh accepted operator call policy; the previous policy was retained"))
	}
}

func (s *Service) writeOperatorCacheLocked(name string, value OperatorSnapshot) error {
	if err := s.writeCache(name, value); err != nil {
		return err
	}
	return s.publishOperatorPolicyLocked()
}

// Reconciliation may suspend a conflicting Site patch while accepting its Zone
// replacement. Commit both documents together so a failed write or a restart
// cannot expose only one half of that decision. A nil local leaves it unchanged.
func (s *Store) acceptOperatorSnapshots(local *OperatorSnapshot, upstream OperatorSnapshot) error {
	type document struct {
		name string
		raw  []byte
	}
	documents := []document{}
	if local != nil {
		raw, err := json.Marshal(local)
		if err != nil {
			return err
		}
		documents = append(documents, document{"operator-local", raw})
	}
	raw, err := json.Marshal(upstream)
	if err != nil {
		return err
	}
	documents = append(documents, document{"operator-upstream", raw})
	for _, doc := range documents {
		if len(doc.raw) > maxConfigurationBytes {
			return errors.New("operator state exceeds the supported cache size")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := nowStamp()
	for _, doc := range documents {
		if _, err = tx.Exec("INSERT INTO framework_documents(name,document,at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET document=excluded.document,at=excluded.at WHERE framework_documents.document <> excluded.document", "cache:"+doc.name, doc.raw, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}
