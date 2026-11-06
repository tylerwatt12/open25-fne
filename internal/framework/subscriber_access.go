package framework

import "slices"

// These rules apply only to physical subscribers. Console, mobile and automation
// permissions continue to be checked against their authenticated identities.
func subscriberRuleAllows(mode string, ids []int, rid int) bool {
	switch mode {
	case "", "all":
		return true
	case "selected":
		return slices.Contains(ids, rid)
	}
	return false
}

func subscriberGroupAllowed(system SystemConfig, rid, tg int, transmit bool) bool {
	physical := slices.ContainsFunc(system.Radios, func(r Radio) bool {
		return r.RID == rid && r.Enabled && r.Kind == "subscriber"
	})
	if !physical {
		return false
	}
	for _, g := range system.Talkgroups {
		if g.TGID == tg && g.Enabled {
			return subscriberRuleAllows(g.AffiliationAccess, g.AffiliationRadioIDs, rid) &&
				(!transmit || subscriberRuleAllows(g.TransmitAccess, g.TransmitRadioIDs, rid))
		}
	}
	return false
}

func (s *Service) hasRFInterest(tg int) bool {
	if s.radio == nil || !s.radio.HasRFInterest(tg) {
		return false
	}
	view := s.view()
	for _, affiliation := range s.radio.Status().Affiliations {
		if affiliation.TGID == tg && !s.radioServiceInhibited(affiliation.RID) && subscriberGroupAllowed(view.System, affiliation.RID, tg, false) {
			return true
		}
	}
	return false
}
