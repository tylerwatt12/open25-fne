package framework

import "math/big"

// Phase 1 FDMA, AABC-E-2019 §2.3.9.2, PDF30 / printed11.
// Site TX = base + channel * spacing; Site RX = Site TX + SU transmit offset.
// Saving settings never calls this operation-readiness calculation.
func bandPlanFrequencies(zone *ZoneConfig, identifier, channel int) (int64, int64, bool) {
	if zone == nil {
		return 0, 0, false
	}
	for _, plan := range zone.ChannelIdentifiers {
		if plan.Identifier != identifier {
			continue
		}
		tx := new(big.Int).Mul(big.NewInt(int64(channel)), big.NewInt(plan.SpacingHz))
		tx.Add(tx, big.NewInt(plan.BaseHz))
		rx := new(big.Int).Add(tx, big.NewInt(plan.OffsetHz))
		if !tx.IsInt64() || !rx.IsInt64() {
			return 0, 0, false
		}
		return tx.Int64(), rx.Int64(), true
	}
	return 0, 0, false
}
