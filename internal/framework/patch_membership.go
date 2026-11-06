package framework

import (
	"errors"
	"slices"
	"sort"
	"time"
)

// HTTP/service admission can precede waiting for patchMu or configuration
// storage. Recheck the authenticated identity after those waits. A queued
// request may use only rights held both when admitted and when executed.
func (s *Service) currentPatchAuthority(initial operatorAuthority) (operatorAuthority, error) {
	current, err := s.operatorActor(initial.actor)
	if err != nil {
		return operatorAuthority{}, err
	}
	current.events = initial.events
	current.user.Talkgroups = slices.DeleteFunc(slices.Clone(current.user.Talkgroups), func(group int) bool {
		return !slices.Contains(initial.user.Talkgroups, group)
	})
	current.user.Permissions = slices.DeleteFunc(slices.Clone(current.user.Permissions), func(permission string) bool {
		return !slices.Contains(initial.user.Permissions, permission)
	})
	return current, nil
}

// A membership update preserves the patch's identity, creator, ownership and
// approved traffic key. UpdatedAt/By describe an accepted membership operation;
// zero means no membership edit has occurred. They are not operator revisions.
func samePatchIdentity(a, b RuntimePatch) bool {
	return a.ID == b.ID && a.ScopeID == b.ScopeID && a.Name == b.Name && a.At.Equal(b.At) &&
		a.CreatedBy == b.CreatedBy && a.CreatedRadioID == b.CreatedRadioID && a.PositionID == b.PositionID &&
		a.AlgorithmID == b.AlgorithmID && a.KeyID == b.KeyID
}

func mergePatchRecord(old, incoming RuntimePatch) RuntimePatch {
	value := old
	if incoming.UpdatedAt.After(old.UpdatedAt) {
		value = incoming
	}
	// A retained end or Site-local conflict cannot be undone by a delayed copy.
	for _, prior := range []RuntimePatch{old, incoming} {
		if !prior.EndedAt.IsZero() && (value.EndedAt.IsZero() || prior.EndedAt.Before(value.EndedAt)) {
			value.EndedAt, value.EndedBy = prior.EndedAt, prior.EndedBy
		}
		if prior.State == "conflict" {
			value.State, value.Reason = prior.State, prior.Reason
		}
	}
	if !value.EndedAt.IsZero() {
		value.Active, value.State = false, "ended"
	}
	value.Talkgroups = slices.Clone(value.Talkgroups)
	value.CanEdit = false // derived anew for each authorized API response
	return value
}

// An upstream snapshot still replaces its cache, including omissions. For a
// present patch, however, an older copy cannot undo an accepted owner edit/end.
func retainNewerPatchRecords(incoming, known []RuntimePatch) []RuntimePatch {
	result := make([]RuntimePatch, 0, len(incoming))
	for _, patch := range incoming {
		patch.CanEdit = false
		for _, old := range known {
			if old.ID == patch.ID {
				if samePatchIdentity(old, patch) {
					patch = mergePatchRecord(old, patch)
				} else {
					patch = old
				}
				break
			}
		}
		result = append(result, patch)
	}
	return result
}

func (s *Service) patchEditAllowed(patch RuntimePatch, authority operatorAuthority) bool {
	role := s.Config().Role
	return (role == RoleZone || role == RoleSite) && patch.ScopeID == s.Config().InstanceID &&
		operatorPermitted(authority, "patch") && patch.Active && patch.EndedAt.IsZero() && patch.State == "active" &&
		(patch.AlgorithmID == 0 || operatorPermitted(authority, "secure_ptt")) &&
		s.patchPolicy(patch) == nil && !s.patchMembersBusy(patch.Talkgroups)
}

func sortedPatchGroups(groups []int) []int {
	value := slices.Clone(groups)
	sort.Ints(value)
	return value
}

// applyOperator holds patchMu then featureMu, excluding new broker admission
// through the durable write and publication of the whole replacement policy.
func (s *Service) updatePatchMembership(local *OperatorSnapshot, combined OperatorSnapshot, input OperatorInput, authority operatorAuthority, now time.Time) error {
	if input.Patch == nil || input.ID == "" {
		return errors.New("select a current patch to edit")
	}
	for _, current := range combined.Patches {
		if current.ID != input.ID {
			continue
		}
		if current.ScopeID != s.Config().InstanceID {
			return errors.New("edit this patch at the controller that owns it")
		}
		if !current.Active || !current.EndedAt.IsZero() || current.State != "active" {
			return errors.New("this patch is not active; refresh the patch list")
		}
		value := input.Patch
		if value.Name != current.Name || value.AlgorithmID != current.AlgorithmID || value.KeyID != current.KeyID {
			return errors.New("only patch membership can be changed")
		}
		if !slices.Equal(sortedPatchGroups(value.PreviousTalkgroups), sortedPatchGroups(current.Talkgroups)) {
			return errors.New("this patch changed; refresh it before editing")
		}
		if current.AlgorithmID != 0 && !operatorPermitted(authority, "secure_ptt") {
			return errors.New("encrypted patch operation is not permitted")
		}
		affected := append(slices.Clone(current.Talkgroups), value.Talkgroups...)
		for _, group := range affected {
			if !s.operatorGroupAllowed(authority, group) {
				return errors.New("every old and new patch member must be permitted")
			}
		}
		next := current
		next.Talkgroups = sortedPatchGroups(value.Talkgroups)
		if err := s.patchPolicy(next); err != nil {
			return err
		}
		if s.patchMembersBusy(affected) {
			return errors.New("wait for current calls to finish before editing this patch")
		}
		for _, other := range combined.Patches {
			if other.ID != current.ID && other.Active && other.EndedAt.IsZero() && overlapGroups(affected, other.Talkgroups) {
				return errors.New("a talkgroup already belongs to another active patch")
			}
		}
		if slices.Equal(next.Talkgroups, current.Talkgroups) {
			return nil
		}
		if !now.After(current.At) {
			now = current.At.Add(time.Nanosecond)
		}
		if !now.After(current.UpdatedAt) {
			now = current.UpdatedAt.Add(time.Nanosecond)
		}
		next.UpdatedAt, next.UpdatedBy, next.CanEdit = now, authority.actor.UserID, false
		setPatch(local, next)
		s.operatorEvent(authority, next.Talkgroups[0], "patch_update", "Patch membership updated.", map[string]any{
			"patch_id": next.ID, "previous_talkgroups": slices.Clone(current.Talkgroups),
			"talkgroups": slices.Clone(next.Talkgroups), "scope_id": next.ScopeID,
			"algorithm_id": next.AlgorithmID, "key_id": next.KeyID,
		})
		return nil
	}
	return errors.New("the patch is not available")
}
