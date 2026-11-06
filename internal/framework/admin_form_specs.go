package framework

type adminFormSection struct {
	Name     string
	Fields   []string
	Advanced bool
}
type adminPageSpec struct {
	Title, Description string
	Sections           []adminFormSection
	Labels             map[string]string
}

var adminPageSpecs = map[string]adminPageSpec{
	"system": {Title: "Radio system identity", Description: "Network identity and generated signaling", Sections: []adminFormSection{
		{Name: "Identity", Fields: []string{"id", "name", "wacn", "system_id"}},
		{Name: "Identification and signaling", Advanced: true, Fields: []string{"callsign", "voice_mfid", "bsi_profile"}},
	}, Labels: map[string]string{"id": "Record ID", "name": "System name", "wacn": "WACN", "system_id": "P25 System ID", "callsign": "Callsign", "voice_mfid": "Generated voice MFID", "bsi_profile": "BSI signaling profile"}},
	"radios": {Title: "Radios", Description: "Radio identities and their directory names", Sections: []adminFormSection{
		{Name: "Radio identity", Fields: []string{"rid", "alias", "group", "kind", "enabled"}},
	}, Labels: map[string]string{"rid": "Radio / Unit ID", "alias": "Name", "kind": "Type", "enabled": "System access enabled"}},
	"talkgroups": {Title: "Talkgroups", Description: "Group identity, access, call handling and encryption", Sections: []adminFormSection{
		{Name: "Identity", Fields: []string{"tgid", "alias", "group", "enabled"}},
		{Name: "Radio participation", Fields: []string{"affiliation_access", "affiliation_radio_ids", "transmit_access", "transmit_radio_ids"}},
		{Name: "Site participation", Fields: []string{"site_access", "site_ids"}},
		{Name: "Call handling", Advanced: true, Fields: []string{"start_policy", "priority"}},
		{Name: "Encryption", Fields: []string{"admission_mode", "transmit_mode", "algorithm_id", "key_id"}},
	}, Labels: map[string]string{"tgid": "Talkgroup ID", "alias": "Name", "enabled": "Enabled", "affiliation_access": "Who may affiliate", "affiliation_radio_ids": "Selected radios for affiliation", "transmit_access": "Who may transmit", "transmit_radio_ids": "Selected radios for transmission", "site_access": "Sites permitted to carry this talkgroup", "site_ids": "Selected Sites", "start_policy": "Call start behavior", "priority": "Routine priority", "admission_mode": "Calls accepted by the system", "transmit_mode": "App / announcement transmit strapping", "algorithm_id": "Transmit algorithm", "key_id": "Transmit Key ID"}},
	"status-labels": {Title: "Status meanings", Description: "", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"id", "label"}},
	}, Labels: map[string]string{"id": "Record ID", "label": "Meaning"}},
	"message-labels": {Title: "Radio message meanings", Description: "", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"id", "label"}},
	}, Labels: map[string]string{"id": "Record ID", "label": "Meaning"}},
	"positions": {Title: "Console positions", Description: "Shared dispatch desk identities", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"id", "name", "radio_id", "enabled", "talkgroups"}},
	}, Labels: map[string]string{"id": "Record ID", "name": "Position name", "radio_id": "Console Radio ID (decimal)", "enabled": "Enabled", "talkgroups": "Assigned talkgroups"}},
	"zone": {Title: "Zone identity", Description: "Local RF subsystem identity", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"id", "system_id", "name", "rfss_id"}},
	}, Labels: map[string]string{"id": "Record ID", "system_id": "Parent System record ID", "name": "Zone name", "rfss_id": "RFSS ID"}},
	"identifiers": {Title: "Band Plan", Description: "How a channel number becomes an RF frequency", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"identifier", "base_frequency_hz", "channel_spacing_hz", "uplink_offset_hz", "bandwidth_hz"}},
	}, Labels: map[string]string{"identifier": "Band Plan / Channel Identifier", "base_frequency_hz": "Base frequency (MHz)", "channel_spacing_hz": "Channel spacing (kHz)", "uplink_offset_hz": "Subscriber transmit offset (MHz)", "bandwidth_hz": "Bandwidth (kHz)"}},
	"sites": {Title: "Site registry", Description: "P25 Site identities and controller addresses", Sections: []adminFormSection{
		{Name: "Site identity", Fields: []string{"id", "name", "site_id", "nac", "lra"}},
		{Name: "Controller addresses", Fields: []string{"endpoint", "admin_url"}},
	}, Labels: map[string]string{"id": "Record ID", "name": "Site name", "site_id": "P25 Site ID", "lra": "Location Registration Area (LRA)", "nac": "NAC", "endpoint": "Internal service address", "admin_url": "Administration address"}},
	"modems": {Title: "USB modems", Description: "Stable USB assignments and calibration", Sections: []adminFormSection{
		{Name: "USB assignment", Fields: []string{"id", "name", "device", "baud"}},
		{Name: "Frequency correction", Advanced: true, Fields: []string{"calibration.rx_offset_hz", "calibration.tx_offset_hz"}},
		{Name: "Signal levels", Advanced: true, Fields: []string{"calibration.rx_level", "calibration.tx_level", "calibration.rx_dc_offset", "calibration.tx_dc_offset"}},
		{Name: "Polarity", Advanced: true, Fields: []string{"calibration.rx_invert", "calibration.tx_invert", "calibration.ptt_invert"}},
	}, Labels: map[string]string{"id": "Record ID", "name": "Modem name", "device": "Stable USB identity", "baud": "Serial rate (baud)", "calibration.rx_offset_hz": "Receive frequency correction (Hz)", "calibration.tx_offset_hz": "Transmit frequency correction (Hz)", "calibration.rx_level": "Receive level (%)", "calibration.tx_level": "Transmit level (%)", "calibration.rx_dc_offset": "Receive DC offset", "calibration.tx_dc_offset": "Transmit DC offset", "calibration.rx_invert": "Invert receive", "calibration.tx_invert": "Invert transmit", "calibration.ptt_invert": "Invert PTT"}},
	"channels": {Title: "RF channels", Description: "Frequencies, modem assignment and channel roles", Sections: []adminFormSection{
		{Name: "Channel", Fields: []string{"id", "name", "enabled", "role"}},
		{Name: "RF programming", Fields: []string{"identifier", "channel_no", "tx_frequency_hz", "rx_frequency_hz", "power_percent"}},
		{Name: "Modem", Fields: []string{"modem_id"}},
		{Name: "Control selection", Advanced: true, Fields: []string{"control_eligible", "control_priority"}},
	}, Labels: map[string]string{"id": "Record ID", "name": "Channel name", "role": "Assigned role", "control_eligible": "Eligible for control", "control_priority": "Control selection order", "identifier": "Channel Identifier", "channel_no": "P25 channel number", "tx_frequency_hz": "Site transmit frequency (MHz)", "rx_frequency_hz": "Site receive frequency (MHz)", "power_percent": "Transmit power (%)", "modem_id": "Assigned modem", "enabled": "Enabled"}},
	"control-policy": {Title: "Control channel behavior", Description: "Failure replacement and scheduled rotation", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"control_policy.mode", "control_policy.rotation_enabled", "control_policy.rotation_hours"}},
		{Name: "Identifier broadcasts", Advanced: true, Fields: []string{"broadcast_band_plan"}},
	}, Labels: map[string]string{"control_policy.mode": "Control assignment", "control_policy.rotation_enabled": "Scheduled control rotation", "control_policy.rotation_hours": "Rotation interval (hours)"}},
	"identification": {Title: "Station identification", Description: "Automatic CWID and BSI settings", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"enabled", "mode", "wpm", "quiet_seconds"}},
	}, Labels: map[string]string{"enabled": "Automatic identification enabled", "mode": "Identification mode", "wpm": "Morse speed (WPM)", "quiet_seconds": "Quiet period (seconds)"}},
	"application-settings": {Title: "Application Server", Description: "Application identity and controller connections", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"name", "connection_url", "local_site_url"}},
	}, Labels: map[string]string{"name": "Application Server name", "connection_url": "Controller address", "local_site_url": "Local Site address"}},
	"networking": {Title: "Connections", Description: "Addresses and settings synchronization", Sections: []adminFormSection{
		{Name: "Addresses", Fields: []string{"public_url", "internal_url", "upstream_url"}},
		{Name: "Synchronization", Advanced: true, Fields: []string{"poll_seconds"}},
	}, Labels: map[string]string{"public_url": "Web administration address", "internal_url": "Internal service address", "upstream_url": "Upstream controller address", "poll_seconds": "Settings synchronization interval (seconds)"}},
	"account": {Title: "Administrator password", Description: "The shared administrator account", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"current_password", "new_password"}},
	}, Labels: map[string]string{"current_password": "Current password", "new_password": "New password"}},
	"radio-commands": {Title: "Radio commands", Description: "Explicit requests to a radio, with separate delivery and reply results", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"action", "radio_id", "code", "site_id"}},
	}, Labels: map[string]string{"action": "Command", "radio_id": "Radio / Unit ID (decimal)", "code": "Message code (decimal)", "site_id": "Transmitting Site"}},
	"radio-operation": {Title: "Channel operation", Description: "Start, stop and switch the operating control channel", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"channel_id", "target_channel_id"}},
	}, Labels: map[string]string{"channel_id": "Channel record ID", "target_channel_id": "New control channel"}},
	"users": {Title: "Login accounts", Description: "People, application roles and assigned radio identities", Sections: []adminFormSection{
		{Name: "Login", Fields: []string{"id", "username", "password", "enabled"}},
		{Name: "Radio and desk identity", Fields: []string{"radio_id", "position_ids", "role"}},
		{Name: "Access", Fields: []string{"talkgroups", "permissions"}},
	}, Labels: map[string]string{"id": "Record ID", "username": "Username", "password": "Password", "radio_id": "Radio / Unit ID (decimal)", "position_ids": "Assigned console positions", "role": "Application role", "enabled": "Enabled", "talkgroups": "Assigned talkgroups", "permissions": "Radio permissions"}},
	"profiles": {Title: "Saved operator profiles", Description: "Zones, talkgroups and receive preferences for each account", Sections: []adminFormSection{
		{Name: "Profile", Fields: []string{"name", "preferred_position_id", "selected_talkgroup", "transmit_talkgroups"}},
		{Name: "Zone layout", Fields: []string{"zones"}},
		{Name: "Receive audio", Fields: []string{"receive"}},
	}, Labels: map[string]string{"name": "Profile name", "preferred_position_id": "Preferred console position", "selected_talkgroup": "Selected talkgroup", "transmit_talkgroups": "Transmit talkgroups", "zone_lines": "Zones and channel order", "receive_lines": "Receive settings", "zones": "Zones and channel order", "receive": "Receive settings"}},
	"automations": {Title: "Automation clients", Description: "Non-human identities used for announcements and integrations", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"id", "name", "radio_id", "enabled", "talkgroups", "actions"}},
	}, Labels: map[string]string{"id": "Record ID", "name": "Name", "radio_id": "Radio / Unit ID (decimal)", "enabled": "Enabled", "talkgroups": "Assigned talkgroups", "actions": "Actions (comma separated)"}},
	"keys": {Title: "Traffic keys", Description: "Traffic-key identities and deliberate key loading", Sections: []adminFormSection{
		{Name: "Key identity", Fields: []string{"algorithm_id", "key_id", "name", "enabled"}},
		{Name: "Key loading", Fields: []string{"material"}},
	}, Labels: map[string]string{"algorithm_id": "Encryption algorithm", "key_id": "Transmit Key ID", "name": "Key name", "enabled": "Enabled", "material": "Load or replace key material (hex)"}},
	"recipients": {Title: "Application key enrollment", Description: "Public identities authorized to receive sealed traffic keys", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"recipient", "public_key"}},
	}, Labels: map[string]string{"recipient": "Application Server identity", "public_key": "Public enrollment key"}},
	"history-settings": {Title: "History storage", Description: "Retention and stored-history limits", Sections: []adminFormSection{
		{Name: "Stored history", Fields: []string{"retention_days", "max_events", "max_calls", "max_locations"}},
		{Name: "Location reports", Fields: []string{"location_stale_seconds"}},
	}, Labels: map[string]string{"retention_days": "Retention (days)", "max_events": "Maximum events", "max_calls": "Maximum calls", "max_locations": "Maximum location reports", "location_stale_seconds": "Location becomes stale after (seconds)"}},
	"history": {Title: "History", Description: "Search recorded calls, events and location reports", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"since", "until", "radio_id", "talkgroup_id", "site_id", "channel_id", "call_id", "type", "source_kind", "audience", "limit"}},
	}, Labels: map[string]string{"since": "From (date and time)", "until": "Until (date and time)", "radio_id": "Radio ID", "talkgroup_id": "Talkgroup ID", "site_id": "P25 Site ID", "channel_id": "Channel ID", "call_id": "Call ID", "type": "Event type", "source_kind": "Source", "audience": "Audience", "limit": "Records per page"}},
	"c4fm-test": {Title: "C4FM transmitter test", Description: "A timed transmit test using a configured voice transmitter", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"channel_id", "duration_seconds"}},
	}, Labels: map[string]string{"channel_id": "Transmitter channel", "duration_seconds": "Duration (seconds)"}},
	"site-identity": {Title: "Local Site identity", Description: "Local identity and links to Zone-owned programming", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"id"}},
	}, Labels: map[string]string{"id": "Record ID"}},
	"alarm-settings": {Title: "Alarm settings", Description: "Planned backend · Named alarm conditions and their local settings", Sections: []adminFormSection{
		{Name: "Alarm identity", Fields: []string{"id", "name", "enabled"}},
		{Name: "Condition and element", Fields: []string{"element_type", "element_id", "category", "severity"}},
		{Name: "Timing", Fields: []string{"raise_delay_seconds", "recovery_delay_seconds"}},
	}, Labels: map[string]string{"id": "Alarm identity", "name": "Alarm name", "enabled": "Monitoring enabled", "element_type": "Element type", "element_id": "Element identity", "category": "Category", "severity": "Assigned severity", "raise_delay_seconds": "Raise delay (seconds)", "recovery_delay_seconds": "Recovery delay (seconds)"}},
	"notification-rules": {Title: "Notification rules", Description: "Planned backend · Which alarm changes request email", Sections: []adminFormSection{
		{Name: "Rule", Fields: []string{"id", "name", "enabled", "scope"}},
		{Name: "Alarm selection", Fields: []string{"alarm_ids", "categories", "severities"}},
		{Name: "Notification changes", Fields: []string{"transitions"}},
	}, Labels: map[string]string{"id": "Rule identity", "name": "Rule name", "enabled": "Rule enabled", "scope": "Alarm scope", "alarm_ids": "Named alarm identities", "categories": "Categories", "severities": "Severities", "transitions": "Send when"}},
	"smtp-settings": {Title: "SMTP settings", Description: "Local email profile and parent-sent inheritance", Sections: []adminFormSection{
		{Name: "Profile source", Fields: []string{"enabled"}},
		{Name: "Local SMTP server", Fields: []string{"host", "port", "authentication", "username", "smtp_password"}},
		{Name: "Local sender and recipients", Fields: []string{"sender_name", "sender_address", "recipients"}},
	}, Labels: map[string]string{"enabled": "Local profile enabled", "host": "SMTP server", "port": "Port", "authentication": "Authentication", "username": "Username", "smtp_password": "Replace password", "sender_name": "Sender name", "sender_address": "Sender address", "recipients": "Recipients"}},
	"alarm-actions": {Title: "Alarm acknowledgment", Description: "Explicitly record that an infrastructure alarm has been seen", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"alarm_id"}},
	}, Labels: map[string]string{"alarm_id": "Alarm identity"}},
	"notification-actions": {Title: "Email test and retry", Description: "Explicit actions using saved notification settings", Sections: []adminFormSection{
		{Name: "Settings", Fields: []string{"operation", "delivery_id"}},
	}, Labels: map[string]string{"operation": "Operation", "delivery_id": "Delivery identity"}},
}
