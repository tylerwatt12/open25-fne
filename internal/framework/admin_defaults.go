package framework

// Starting choices for a new control-selection policy.
// This map is used only when opening a policy that has never been configured.
func webNewControlPolicy() map[string]any {
	return map[string]any{"mode": "static", "rotation_enabled": false, "rotation_hours": 24}
}

// Starting values for an Add form. Never apply them to a saved
// record or submitted form; every choice remains editable.
func webNewRecord(kind string) map[string]any {
	switch kind {
	case "radios":
		return map[string]any{"kind": "subscriber", "enabled": true}
	case "talkgroups":
		return map[string]any{
			"enabled": true, "affiliation_access": "all", "transmit_access": "all", "site_access": "all",
			"start_policy": "fast_start", "priority": 10, "admission_mode": "clear", "transmit_mode": "clear",
		}
	case "users":
		return map[string]any{"role": "operator", "enabled": true}
	case "positions", "automations":
		return map[string]any{"enabled": true}
	case "channels":
		return map[string]any{"role": "voice", "enabled": false}
	case "modems":
		return map[string]any{"baud": 115200, "calibration": map[string]any{
			"rx_offset_hz": 0, "tx_offset_hz": 0, "rx_dc_offset": 0, "tx_dc_offset": 0,
			"rx_invert": false, "tx_invert": false, "ptt_invert": false,
		}}
	case "identifiers":
		return map[string]any{"channel_spacing_hz": 12500, "bandwidth_hz": 12500}
	case "keys":
		return map[string]any{"enabled": false}
	}
	return map[string]any{}
}
