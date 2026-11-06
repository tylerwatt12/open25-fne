package radio

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var nativeFields = regexp.MustCompile(`\b(srcId|dstId|tgt|arg|op|serviceType|status|message|si1|si2|attempts|reason)\s*=\s*(\$[0-9A-Fa-f]+|[0-9]+)\b`)
var nativeTime = regexp.MustCompile(`^[A-Z]: ([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3})`)
var activityVoice = regexp.MustCompile(`P25, (RF|network) (encrypted )?voice transmission from ([0-9]+) to (?:TG )?([0-9]+)`)

// ParseEvent records observations, never an intended command outcome. In
// particular only accepted native confirmations or table changes establish
// registration and affiliation. Repeated control broadcasts are not events.
func ParseEvent(line string) *Event {
	if !strings.Contains(line, "P25") {
		return nil
	}
	e := &Event{At: time.Now().UTC(), Audience: "operator", Direction: "received", Message: line}
	if stamp := nativeTime.FindStringSubmatch(line); len(stamp) > 1 {
		e.NativeAt = stamp[1]
	}
	values := map[string]int{}
	for _, m := range nativeFields.FindAllStringSubmatch(line, -1) {
		base, text := 10, m[2]
		if strings.HasPrefix(text, "$") {
			base, text = 16, text[1:]
		}
		v, err := strconv.ParseInt(text, base, 32)
		if err == nil {
			values[m[1]] = int(v)
		}
	}
	e.RID = values["srcId"]
	e.TGID = values["dstId"]
	if strings.Contains(line, "P25 Voice Call,") && (strings.Contains(line, "(RF)") || strings.Contains(line, "(NET)")) {
		e.Kind = "call_started"
		if strings.Contains(line, "(NET)") {
			e.Direction = "sent"
		}
		return e
	}
	if m := activityVoice.FindStringSubmatch(line); len(m) > 0 {
		e.Kind = "call_started"
		e.RID, _ = strconv.Atoi(m[3])
		e.TGID, _ = strconv.Atoi(m[4])
		e.Encrypted = m[2] != ""
		if m[1] == "network" {
			e.Direction = "sent"
		}
		return e
	}
	if strings.Contains(line, "P25, RF end of transmission") || strings.Contains(line, "P25, network end of transmission") {
		e.Kind = "call_ended"
		if strings.Contains(line, "network") {
			e.Direction = "sent"
		}
		return e
	}
	if strings.Contains(line, "(HOST) P25 Message Update, acknowledged,") || strings.Contains(line, "(HOST) P25 Message Update, no response,") {
		code, ok := values["message"]
		if !ok || code > 65535 || values["arg"] != NativeFNEID || values["attempts"] != 1 {
			return nil
		}
		e.Kind, e.RID, e.TGID = "message_ack", values["tgt"], 0
		e.RequestorID, e.Attempts, e.Code = values["arg"], values["attempts"], &code
		if strings.Contains(line, "no response,") {
			e.Kind, e.Direction = "message_no_response", "state"
		}
	} else if strings.Contains(line, "(HOST) P25 Status Query, no response,") {
		if values["arg"] != NativeFNEID || values["attempts"] != 1 {
			return nil
		}
		e.Kind, e.Direction = "status_query_no_response", "state"
		e.RID, e.TGID = values["tgt"], 0
		e.RequestorID, e.Attempts = values["arg"], values["attempts"]
		e.Message = "No matching subscriber status response before the Site's response deadline."
	} else if strings.Contains(line, "(HOST) P25 Radio Check, no response,") || strings.Contains(line, "(HOST) P25 Radio Inhibit, no response,") || strings.Contains(line, "(HOST) P25 Radio Uninhibit, no response,") {
		// This is a native request-state outcome, not an RF response or a
		// declaration that the subscriber is absent/deregistered.
		if values["arg"] != NativeFNEID || values["attempts"] != 5 {
			return nil
		}
		e.Kind, e.Direction = "radio_check_no_response", "state"
		e.RID, e.TGID = values["tgt"], 0
		e.RequestorID, e.Attempts = values["arg"], values["attempts"]
		e.Message = "No subscriber response after the Site's radio-check retries."
		if strings.Contains(line, "P25 Radio Inhibit,") {
			e.Kind = "inhibit_no_response"
			e.Message = "No subscriber response after the Site's inhibit retries; local inhibition remains enforced."
		}
		if strings.Contains(line, "P25 Radio Uninhibit,") {
			e.Kind = "uninhibit_no_response"
			e.Message = "No subscriber response after the Site's re-enable retries; local inhibition remains pending re-enable."
		}
	} else if strings.Contains(line, "(HOST) P25 Affiliation,") {
		e.Direction = "state"
		switch {
		case strings.Contains(line, ", unit registration,"):
			e.Kind = "registered"
		case strings.Contains(line, ", unit deregistration,"):
			e.Kind = "deregistered"
		case strings.Contains(line, ", group affiliation,"):
			e.Kind = "affiliated"
		case strings.Contains(line, ", group unaffiliation,"):
			e.Kind = "unaffiliated"
		default:
			return nil
		}
		e.Audience = "system_operator"
	} else if strings.Contains(line, "(RF)") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "denial") || strings.Contains(lower, "rejection") || strings.Contains(lower, "affiliation query mismatch") {
			e.Kind = "request_denied"
			e.Audience = "system_operator"
			switch {
			case strings.Contains(lower, "affiliation query mismatch"):
				e.Message = "Radio affiliation does not match the Site's record."
			case strings.Contains(lower, "emergency alarm denial"):
				if reason, ok := values["reason"]; ok {
					e.Code = &reason
					e.Message = map[int]string{
						0x10: "Emergency alarm rejected: the requesting radio is unavailable or not registered.",
						0x30: "Emergency alarm rejected: this talkgroup is unavailable at this Site.",
						0x31: "Emergency alarm rejected: this radio is not admitted to the talkgroup at this Site.",
					}[reason]
					if e.Message == "" {
						e.Message = line
					}
				}
			case strings.Contains(lower, "physical subscriber denial"):
				e.Message = "Radio ID is not assigned to an enabled physical subscriber."
			case strings.Contains(lower, "subscriber transmit denial"):
				e.Message = "Subscriber is not permitted to transmit on this talkgroup."
			case strings.Contains(lower, "subscriber affiliation denial"), strings.Contains(lower, "subscriber location affiliation denial"):
				e.Message = "Subscriber is not permitted to affiliate with this talkgroup."
			}
		} else {
			switch {
			case strings.Contains(line, "P25, TSDU (Trunking System Data Unit), Emergency Alarm Cancel Accepted,"):
				// Only the native handler's accepted, ACK-queued outcome clears
				// an incident. Its optional CAN_SRV_REQ diagnostic is not proof.
				if e.TGID < 1 || e.TGID > 65534 {
					return nil
				}
				e.Kind = "emergency_cancel"
				e.Message = "Radio cancelled its emergency alarm."
			case strings.Contains(line, "(Unit Registration Response)"):
				e.Kind = "registered"
				e.Audience = "system_operator"
				e.Direction = "state"
			case strings.Contains(line, "(Group Affiliation Response)") || strings.Contains(line, "(Location Registration Response)") || strings.Contains(line, "(Group Affiliation Query Response)"):
				e.Kind = "affiliated"
				e.Audience = "system_operator"
				e.Direction = "state"
			case strings.Contains(line, "(Emergency Alarm Request)"):
				e.Kind = "emergency"
				if v, ok := values["si1"]; ok && v <= 255 {
					e.EmergencyInfo1 = &v
				}
				if v, ok := values["si2"]; ok && v <= 255 {
					e.EmergencyInfo2 = &v
				}
			case strings.Contains(line, "(Status Query Response)"):
				v, ok := values["status"]
				if !ok || v > 65535 || values["dstId"] != NativeFNEID {
					return nil
				}
				e.Kind, e.RequestorID, e.TGID, e.Code = "status_query_response", values["dstId"], 0, &v
			case strings.Contains(line, "(Status Update Request)"):
				e.Kind = "status"
				if v, ok := values["status"]; ok && v <= 65535 {
					e.Code = &v
				} else {
					return nil
				}
			case strings.Contains(line, "(Message Update Request)"):
				e.Kind = "message"
				if v, ok := values["message"]; ok && v <= 65535 {
					e.Code = &v
				} else {
					return nil
				}
			case strings.Contains(line, "(Call Alert Request)"):
				e.Kind = "page_request"
			case strings.Contains(line, "(Call Alert)"):
				e.Kind = "page"
				e.Direction = "sent"
				e.RID = values["dstId"]
			case strings.Contains(line, "(Extended Function Response)"):
				e.RID = values["tgt"]
				e.RequestorID = values["arg"]
				code := values["op"]
				e.Code = &code
				e.Kind = map[int]string{0x80: "radio_check_ack", 0xff: "inhibit_ack", 0xfe: "uninhibit_ack", 0x280: "regroup_ack", 0x281: "regroup_cancel_ack"}[code]
			case strings.Contains(line, "(Extended Function Command)"):
				e.RID = values["tgt"]
				e.Direction = "sent"
				code := values["op"]
				e.Code = &code
				e.Kind = map[int]string{0: "radio_check", 0x7f: "inhibit", 0x7e: "uninhibit", 0x200: "regroup", 0x201: "regroup_cancel"}[code]
			case strings.Contains(line, "(Group Affiliation Query)"):
				e.Kind = "affiliation_query"
				e.Direction = "sent"
				e.RID = values["dstId"]
			case strings.Contains(line, "(Unit Registration Command)"):
				e.Kind = "registration_demand"
				e.Direction = "sent"
				e.RID = values["dstId"]
			case strings.Contains(line, "(Acknowledge Response - Unit)"):
				e.Kind = "acknowledgement"
				if values["serviceType"] == 0x1f {
					e.Kind = "page_ack"
				}
			default:
				return nil
			}
		}
	} else {
		return nil
	}
	if e.Kind == "" || e.RID < 1 || e.RID > 0xFFFFFF {
		return nil
	}
	return e
}

func identificationLine(line string) (string, bool) {
	for _, state := range []string{"idle", "quiet", "draining", "identifying", "restoring"} {
		if strings.HasSuffix(strings.TrimSpace(line), "CW, state "+state) {
			return state, true
		}
	}
	return "", false
}
func sensitiveLine(line string) bool {
	lower := strings.ToLower(line)
	for _, word := range []string{"password", "secret", "unique id", "udid", "auth token", "authentication token"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}
