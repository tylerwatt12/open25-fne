package framework

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

var adminFieldHints = map[string]string{
	"talkgroups.admission_mode":     "Which clear or encrypted calls this talkgroup accepts.",
	"talkgroups.transmit_mode":      "The transmit choice used by apps and announcements.",
	"talkgroups.site_access":        "Where calls on this talkgroup may be carried.",
	"talkgroups.requested_site_ids": "Carry calls at these Sites when a voice channel is available, even without affiliated radios. An empty selection uses affiliations. Site access still applies.",
	"talkgroups.algorithm_id":       "Default transmit algorithm. Operators can select another loaded key.",
	"talkgroups.key_id":             "The default key identity, stored and loaded separately.",
	"talkgroups.start_policy":       "FASTStart starts promptly; AllStart waits for participating Sites.",
	"users.radio_id":                "The radio identity used when this person operates PTT.",
	"users.position_ids":            "Shared dispatch desks this person can use.",
	"channels.modem_id":             "The local USB modem that serves this channel.",
	"modems.device":                 "Use the stable USB identity, independent of tty numbering.",
	"keys.material":                 "Enter new material only when deliberately loading or replacing a key.",
}

type adminChoice struct {
	ID, Name  string
	DisplayID string `json:",omitempty"`
	InUse     string `json:",omitempty"`
	Disabled  bool   `json:",omitempty"`
	BaseHz    string `json:",omitempty"`
	SpacingHz string `json:",omitempty"`
	OffsetHz  string `json:",omitempty"`
}

func (s *Service) adminEditorFields(w io.Writer, kind string, fields []webField, record map[string]any, submitted ...url.Values) {
	fields = append([]webField(nil), fields...)
	names := s.adminNames()
	related := map[string]string{"radio_id": "radios", "talkgroup_id": "talkgroups", "user_id": "users", "affiliation_radio_ids": "radios", "transmit_radio_ids": "radios", "talkgroups": "talkgroups", "transmit_talkgroups": "talkgroups", "selected_talkgroup": "talkgroups", "site_ids": "sites", "requested_site_ids": "sites", "neighbor_site_ids": "neighbor-sites", "site_id": "sites", "modem_id": "modems", "channel_id": "channels", "control_policy.channel_id": "channels", "control_policy.candidate_ids": "channels", "target_channel_id": "channels", "position_ids": "positions", "preferred_position_id": "positions", "zones": "talkgroups", "receive": "talkgroups"}
	for i := range fields {
		if kind == "sites" && fields[i].Name == "id" {
			fields[i].ChoiceKind = "site-instances"
			fields[i].Related = true
			fields[i].Label = "Site controller"
		}
		if adminManagedID(kind, fields[i].Name) {
			fields[i].Kind = "internal"
		}
		if fields[i].Kind == "record-combo" {
			fields[i].ChoiceKind = kind + "-groups"
		}
		fields[i].Hint = adminFieldHints[kind+"."+fields[i].Name]
		if kind == "modems" && fields[i].Name == "device" && s.Config().Role == RoleSite {
			fields[i].ChoiceKind = "usb"
			fields[i].Related = true
		}
		if kind == "talkgroups" && fields[i].Name == "key_id" {
			fields[i].ChoiceKind = "keys"
			fields[i].Related = true
		}

		if fields[i].Name == "permissions" {
			fields[i].Kind = "flags"
			fields[i].Choices = "ptt=Transmit voice|secure_ptt=Transmit encrypted voice|emergency=Declare emergency|patch=Manage patches|radio_check=Request radio check|page=Send call alert|status=Send status|message=Send coded radio message|location_report=Send location|location_view=View locations|location_poll=Request location|emergency_ack=Acknowledge emergency|emergency_clear=Clear emergency|crypto_admin=Manage traffic keys|inhibit=Inhibit radio|uninhibit=Restore inhibited radio|regroup=Regroup radios"
		}

		if label := adminPageSpecs[kind].Labels[fields[i].Name]; label != "" {
			fields[i].Label = label
		}
		if kind == "channels" && fields[i].Name == "identifier" {
			fields[i].ChoiceKind = "bands"
			fields[i].Related = true
		}
		if kind == "zone" && fields[i].Name == "system_id" {
			fields[i].ChoiceKind = "systems"
			fields[i].Related = true
		}
		if key := related[fields[i].Name]; key != "" && !(fields[i].Name == "site_id" && fields[i].Kind == "number") {
			fields[i].ChoiceKind = key
			fields[i].Related = true
		}
	}
	for i := range fields {
		f := &fields[i]
		if f.Related && names[f.ChoiceKind] != nil {
			f.RelationLabels = map[string]string{}
			reference := webText(webValue(record, f.Name))
			if len(submitted) > 0 && submitted[0] != nil {
				reference = submitted[0].Get(f.Name)
			}
			ids := strings.FieldsFunc(reference, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
			for _, id := range ids[:min(len(ids), 50)] {
				id = strings.TrimSpace(id)
				f.RelationLabels[id] = adminName(names, f.ChoiceKind, id)
			}
			if f.Kind == "text" || f.Kind == "strings" {
				f.Multiple = f.Kind == "strings"
				f.Kind = "relation"
			}
		}
	}
	if kind == "channels" {
		idText, chText := webText(record["identifier"]), webText(record["channel_no"])
		if len(submitted) > 0 && submitted[0] != nil {
			idText = submitted[0].Get("identifier")
			chText = submitted[0].Get("channel_no")
		}
		id, e1 := strconv.Atoi(idText)
		ch, e2 := strconv.Atoi(chText)
		tx, rx, known := bandPlanFrequencies(s.webKnownZone(), id, ch)
		if e1 != nil || e2 != nil {
			known = false
		}
		for i := range fields {
			if fields[i].Kind == "derived-frequency" {
				fields[i].Computed = "Unavailable"
				if known {
					hz := tx
					if fields[i].Name == "rx_frequency_hz" {
						hz = rx
					}
					fields[i].Computed = webUnitText(fields[i].Name, hz) + " MHz"
				}
			}
		}
	}
	webEditorFields(w, kind, fields, record, submitted...)
	if kind == "channels" {
		identifier := webText(record["identifier"])
		if len(submitted) > 0 && submitted[0] != nil {
			identifier = submitted[0].Get("identifier")
		}
		if zone := s.webKnownZone(); zone != nil {
			for _, plan := range zone.ChannelIdentifiers {
				if fmt.Sprint(plan.Identifier) == identifier {
					fmt.Fprintf(w, `<div hidden data-band-reference data-identifier="%s" data-base="%d" data-spacing="%d" data-offset="%d"></div>`, esc(identifier), plan.BaseHz, plan.SpacingHz, plan.OffsetHz)
					break
				}
			}
		}
		fmt.Fprintf(w, `<span class=ts-caption data-band-result role=status></span>`)
	}
}
func webFields(w io.Writer, fields []webField, record map[string]any, submitted ...url.Values) {
	fmt.Fprint(w, `<div class=ts-formgrid>`)
	for _, f := range fields {
		if f.Kind == "internal" {
			fmt.Fprintf(w, `<input type=hidden name="%s" value="%s">`, esc(f.Name), esc(webText(webValue(record, f.Name))))
			continue
		}
		value := webValue(record, f.Name)
		var input url.Values
		if len(submitted) > 0 {
			input = submitted[0]
			if input != nil && f.Kind != "password" && f.Kind != "zones" && f.Kind != "receive" {
				value = input.Get(f.Name)
				if f.Kind == "flags" {
					value = strings.Join(input[f.Name], ", ")
				}
				if f.Kind == "bool" {
					value = value == "true"
				}
			}
		}
		wide := ""
		if f.Kind == "lines" || f.Kind == "zones" || f.Kind == "receive" || f.Kind == "flags" || f.Kind == "numbers" || f.Kind == "strings" || f.Name == "device" || f.Name == "public_key" || f.Name == "recipients" {
			wide = " ts-wide"
		}
		label := strings.TrimSuffix(f.Label, " (decimal)")
		fmt.Fprintf(w, `<div class="ts-field%s" data-field="%s"><div class=ts-fieldlabel><label for="f-%s">%s</label>`, wide, esc(f.Name), esc(f.Name), esc(label))
		if f.Help == "" {
			f.Help = f.Hint
		}
		if f.Help != "" {
			fmt.Fprint(w, webHelp(label, f.Help))
		}
		if f.BulkApply {
			checked := ""
			if f.ApplyChecked {
				checked = " checked"
			}
			fmt.Fprintf(w, `<label class=ts-bulkapply><input type=checkbox name=apply value="%s"%s>Apply</label>`, esc(f.Name), checked)
		}
		fmt.Fprint(w, `</div><div class=ts-fieldbody>`)
		switch f.Kind {

		case "relation":
			labels, _ := json.Marshal(f.RelationLabels)
			shown := []string{}
			ids := strings.Split(webText(value), ",")
			for _, id := range ids[:min(len(ids), 50)] {
				id = strings.TrimSpace(id)
				if id != "" {
					label := f.RelationLabels[id]
					if label == "" {
						label = "Unnamed entry"
					}
					shown = append(shown, label)
				}
			}
			if len(ids) > 50 {
				shown = append(shown, fmt.Sprintf("%d more", len(ids)-50))
			}
			if len(shown) == 0 {
				shown = append(shown, "None selected")
			}
			fmt.Fprintf(w, `<div class=ts-relation><input type=hidden name="%s" value="%s" data-choice-labels="%s"><output id="f-%s" data-relation-display>%s</output>`, esc(f.Name), esc(webText(value)), esc(string(labels)), esc(f.Name), esc(strings.Join(shown, ", ")))
			fmt.Fprint(w, `<div class=ts-actions>`)
			webChooserButton(w, f.ChoiceKind, f.Name, f.Multiple)
			fmt.Fprint(w, `<button type=button data-relation-clear>Clear</button></div></div>`)
		case "record-combo":
			fmt.Fprintf(w, `<input id="f-%s" name="%s" value="%s" list="choices-%s" data-record-combo="%s" autocomplete=off><datalist id="choices-%s"></datalist>`, esc(f.Name), esc(f.Name), esc(webText(value)), esc(f.Name), esc(f.ChoiceKind), esc(f.Name))
		case "derived-frequency":
			fmt.Fprintf(w, `<output id="f-%s" class=ts-calculated data-band-frequency="%s">%s</output>`, esc(f.Name), esc(f.Name), esc(f.Computed))
		case "zones", "receive":
			webProfileRows(w, f, value, input)
		case "bool":
			checked := ""
			if value == true {
				checked = " checked"
			}
			text := "Enabled"
			if strings.Contains(f.Name, "invert") {
				text = "Inverted"
			}
			if f.Name == "inherit_smtp" {
				text = "Inherit SMTP configuration from parent"
			}
			fmt.Fprintf(w, `<label class=ts-check><input id="f-%s" type=checkbox name="%s" value=true%s><span>%s</span></label>`, esc(f.Name), esc(f.Name), checked, esc(text))
		case "select-number":
			webNumberPair(w, f, value, input)
			fmt.Fprintf(w, `<div class=ts-select><select data-number-choice aria-label="Named %s"><option value="">Choose by name…</option>`, esc(label))
			for _, choice := range strings.Split(f.Choices, "|") {
				parts := strings.SplitN(choice, "=", 2)
				text := parts[0]
				if len(parts) == 2 {
					text = parts[1]
				}
				selected := ""
				if parts[0] == webText(value) {
					selected = " selected"
				}
				fmt.Fprintf(w, `<option value="%s"%s>%s · %s / %s hex</option>`, esc(parts[0]), selected, esc(text), esc(parts[0]), esc(webHexText(parts[0], webHexWidth(f))))
			}
			fmt.Fprint(w, `</select></div>`)
		case "select":
			fmt.Fprintf(w, `<div class=ts-select><select id="f-%s" name="%s">`, esc(f.Name), esc(f.Name))
			current := webText(value)
			found := false
			for _, c := range strings.Split(f.Choices, "|") {
				if strings.SplitN(c, "=", 2)[0] == current {
					found = true
				}
			}
			if !found {
				fmt.Fprintf(w, `<option selected value="%s">%s</option>`, esc(current), esc(current))
			}
			for _, c := range strings.Split(f.Choices, "|") {
				pair := strings.SplitN(c, "=", 2)
				name := pair[0]
				if len(pair) == 2 {
					name = pair[1]
				}
				if width := webHexWidth(f); width > 0 {
					if notation := webHexText(pair[0], width); notation != "" {
						name = pair[0] + " / " + notation + " · " + name
					}
				}
				selected := ""
				if pair[0] == current {
					selected = " selected"
				}
				fmt.Fprintf(w, `<option value="%s"%s>%s</option>`, esc(pair[0]), selected, esc(name))
			}
			fmt.Fprint(w, `</select></div>`)
		case "flags":
			fmt.Fprintf(w, `<fieldset id="f-%s" class=ts-checkgrid aria-label="%s">`, esc(f.Name), esc(label))
			chosen := strings.FieldsFunc(webText(value), func(c rune) bool { return c == ',' || c == ' ' })
			for _, c := range strings.Split(f.Choices, "|") {
				pair := strings.SplitN(c, "=", 2)
				text := pair[0]
				if len(pair) == 2 {
					text = pair[1]
				}
				checked := ""
				for _, id := range chosen {
					if id == pair[0] {
						checked = " checked"
					}
				}
				fmt.Fprintf(w, `<label><input type=checkbox name="%s" value="%s"%s>%s</label>`, esc(f.Name), esc(pair[0]), checked, esc(text))
			}
			fmt.Fprint(w, `</fieldset>`)
			unknown := []string{}
			for _, id := range chosen {
				known := false
				for _, choice := range strings.Split(f.Choices, "|") {
					if strings.SplitN(choice, "=", 2)[0] == id {
						known = true
					}
				}
				if !known {
					unknown = append(unknown, id)
				}
			}
			if f.Name == "permissions" {
				fmt.Fprintf(w, `<label class=ts-caption>Additional permission identities<input type=text name="%s" value="%s"></label>`, esc(f.Name), esc(strings.Join(unknown, ", ")))
			}
		case "lines":
			fmt.Fprintf(w, `<textarea id="f-%s" name="%s" rows=4>%s</textarea>`, esc(f.Name), esc(f.Name), esc(webText(value)))
		default:
			if f.Kind == "number" && webHexWidth(f) > 0 {
				if f.Related {
					fmt.Fprint(w, `<div class=ts-relation>`)
				}
				webNumberPair(w, f, value, input)
				if f.Related {
					webChooserButton(w, f.ChoiceKind, f.Name, f.Kind == "numbers" || f.Kind == "strings")
					fmt.Fprint(w, `</div>`)
				}
			} else {
				if f.Related {
					fmt.Fprint(w, `<div class=ts-relation>`)
				}
				kind, extra := "text", ""
				if f.Kind == "number" {
					kind, extra = "number", " step=any"
					if input == nil {
						value = webUnitText(f.Name, value)
					}
				}
				if f.Kind == "numbers" && input == nil {
					value = webUnitText(f.Name, value)
				}
				if webDecimalWidth(f.Name) > 0 {
					extra += ` class=ts-numeric`
				}
				if f.Kind == "password" {
					kind, extra, value = "password", " autocomplete=new-password", ""
				}
				if f.Kind == "user-alias" {
					extra = ` required maxlength=14 pattern="[\x20-\x7E]{1,14}" title="1–14 printable ASCII characters"`
				}
				signedOffset := f.Name == "uplink_offset_hz"
				if signedOffset {
					value = strings.TrimPrefix(webText(value), "+")
					sign := "+"
					if webText(value) == "" || strings.HasPrefix(webText(value), "-") {
						sign = ""
					}
					fmt.Fprintf(w, `<div class=ts-signed-number data-signed-offset><span aria-hidden=true>%s</span>`, sign)
				}
				fmt.Fprintf(w, `<input id="f-%s" type=%s name="%s" value="%s"%s>`, esc(f.Name), kind, esc(f.Name), esc(webText(value)), extra)
				if signedOffset {
					fmt.Fprint(w, `</div>`)
				}
				if f.Related {
					webChooserButton(w, f.ChoiceKind, f.Name, f.Kind == "numbers" || f.Kind == "strings")
					fmt.Fprint(w, `</div>`)
				}
			}
		}

		fmt.Fprint(w, `</div></div>`)
	}
	fmt.Fprint(w, `</div>`)
}
func webSaveBar(w io.Writer, cancel string, deletable ...bool) {
	fmt.Fprint(w, `<div class=ts-savebar><span class=ts-muted data-admin-unsaved-label></span><div class=ts-actions>`)
	if len(deletable) > 0 && deletable[0] {
		fmt.Fprint(w, `<button name=action value=delete formnovalidate>Delete entry</button>`)
	}
	fmt.Fprintf(w, `<a class=ts-button data-admin-cancel href="%s">Cancel</a><button class=ts-primary>Save</button></div></div>`, esc(cancel))
}
