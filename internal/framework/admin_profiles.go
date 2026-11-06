package framework

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func webProfileRows(w io.Writer, f webField, value any, submitted url.Values) {
	kind := f.Kind
	rows, _ := value.([]any)
	if submitted != nil {
		rows = nil
		for _, token := range submitted[kind+"_row"] {
			row := map[string]any{"_token": token}
			names := []string{"name", "talkgroups"}
			if kind == "receive" {
				names = []string{"talkgroup_id", "volume", "pan", "muted"}
			}
			for _, name := range names {
				v := submitted.Get(kind + "_" + token + "_" + name)
				if name == "muted" {
					row[name] = v == "true"
				} else {
					row[name] = v
				}
			}
			rows = append(rows, row)
		}
	}
	next := len(rows)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if token, ok := row["_token"].(string); ok {
			if n, err := strconv.Atoi(token); err == nil && n >= next {
				next = n + 1
			}
		}
	}
	fmt.Fprintf(w, `<div data-profile-grid="%s" data-next-row="%d">`, kind, next)
	for i, raw := range rows {
		row, _ := raw.(map[string]any)
		token := fmt.Sprint(i)
		if t, ok := row["_token"].(string); ok {
			token = t
		}
		webProfileRow(w, f, token, row, submitted != nil)
	}
	fmt.Fprint(w, `<template data-profile-template>`)
	webProfileRow(w, f, "__row__", map[string]any{}, false)
	fmt.Fprint(w, `</template>`)
	if kind == "zones" {
		fmt.Fprintf(w, `<template data-zone-controls><button type=button data-zone-action=up aria-label="Move talkgroup up">%s</button><button type=button data-zone-action=down aria-label="Move talkgroup down">%s</button></template>`, adminIcon("arrow-up"), adminIcon("arrow-down"))
	}
	label := "zone"
	if kind == "receive" {
		label = "receive setting"
	}
	fmt.Fprintf(w, `<div class=ts-actions style="margin-top:16px"><button type=button data-profile-add>%sAdd %s</button></div></div>`, adminIcon("plus"), label)
}
func webProfileRow(w io.Writer, f webField, token string, row map[string]any, submitted bool) {
	prefix := f.Kind + "_" + token + "_"
	zone := f.Kind == "zones"
	class := ""
	if !zone {
		class = " ts-receiverow"
	}
	fmt.Fprintf(w, `<div data-profile-row><input type=hidden name="%s_row" value="%s"><div class="ts-gridrow%s">`, f.Kind, esc(token), class)
	input := func(key, label, kind string, value any) {
		fmt.Fprintf(w, `<label>%s<input type=%s step=any name="%s%s" value="%s"></label>`, esc(label), kind, prefix, key, esc(webText(value)))
	}
	if zone {
		input("name", "Zone name", "text", row["name"])
		input("talkgroups", "Talkgroup identities", "text", row["talkgroups"])
	} else {
		fmt.Fprintf(w, `<label>Talkgroup<div class=ts-relation><input type=number step=any name="%stalkgroup_id" value="%s">`, prefix, esc(webText(row["talkgroup_id"])))
		webChooserButton(w, "talkgroups", prefix+"talkgroup_id", false)
		fmt.Fprint(w, `</div></label>`)
		volume := row["volume"]
		if !submitted && volume != nil {
			volume = webUnitText("volume", volume)
		}
		input("volume", "Volume (%)", "number", volume)
		fmt.Fprint(w, `<label>Balance`)
		fmt.Fprintf(w, `<input type=number step=any name="%span" value="%s"><span class=ts-caption>−1 left · 0 center · 1 right</span></label>`, prefix, esc(webText(row["pan"])))
		checked := ""
		if row["muted"] == true {
			checked = " checked"
		}
		fmt.Fprintf(w, `<label class=ts-check><input type=checkbox name="%smuted" value=true%s>Mute</label>`, prefix, checked)
	}
	fmt.Fprintf(w, `<button type=button class=ts-icon data-profile-remove aria-label="Remove %s">%s</button></div>`, map[bool]string{true: "zone", false: "receive setting"}[zone], adminIcon("trash-2"))
	if zone {
		fmt.Fprintf(w, `<div class=ts-orderlist data-zone-order data-zone-target="%stalkgroups"></div><div class=ts-actions>`, prefix)
		fmt.Fprintf(w, `<button type=button data-chooser-kind=talkgroups data-chooser-target="%stalkgroups" data-chooser-multiple=true data-chooser-append=true>Add talkgroups</button></div>`, prefix)

	}
	fmt.Fprint(w, `</div>`)
}
func webReadProfileRows(r *http.Request, kind string) ([]any, error) {
	rows := []any{}
	for _, token := range r.PostForm[kind+"_row"] {
		p := kind + "_" + token + "_"
		row := map[string]any{}
		if kind == "zones" {
			row["name"] = r.FormValue(p + "name")
			ids := []any{}
			for _, id := range strings.FieldsFunc(r.FormValue(p+"talkgroups"), func(c rune) bool { return c == ',' || c == ' ' || c == '\n' || c == '\r' }) {
				if _, err := strconv.ParseInt(id, 10, 64); err != nil {
					return nil, fmt.Errorf("could not read a Talkgroup ID as a number")
				}
				ids = append(ids, json.Number(id))
			}
			row["talkgroups"] = ids
		} else {
			for _, name := range []string{"talkgroup_id", "volume", "pan"} {
				text := r.FormValue(p + name)
				var value any
				if text != "" {
					if _, err := strconv.ParseFloat(text, 64); err != nil {
						return nil, fmt.Errorf("could not read %s as a number", name)
					}
					if name == "volume" {
						scaled, err := webScaleDecimal(text, -2)
						if err != nil {
							return nil, err
						}
						text = scaled
					}
					value = json.Number(text)
				}
				row[name] = value
			}
			row["muted"] = r.FormValue(p+"muted") == "true"
		}
		rows = append(rows, row)
	}
	return rows, nil
}
