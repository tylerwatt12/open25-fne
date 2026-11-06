package framework

import (
	"fmt"
	"io"
	"math/big"
	"net/url"
	"strings"
)

// Width pads a representation; it never limits or changes the saved identity.
func webHexWidth(field webField) int {
	if field.Kind != "number" && field.Kind != "select-number" {
		return 0
	}
	return map[string]int{"wacn": 5, "system_id": 3, "voice_mfid": 2, "rfss_id": 2, "site_id": 2, "lra": 2, "nac": 3, "rid": 6, "radio_id": 6, "tgid": 4, "key_id": 4, "algorithm_id": 2, "identifier": 1, "channel_no": 3, "id": 2, "code": 4, "selected_talkgroup": 4, "profile.selected_talkgroup": 4}[field.Name]
}

func webHexText(text string, width int) string {
	n, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return ""
	}
	sign := ""
	if n.Sign() < 0 {
		sign = "-"
		n.Abs(n)
	}
	text = strings.ToUpper(n.Text(16))
	if len(text) < width {
		text = strings.Repeat("0", width-len(text)) + text
	}
	return sign + text
}

func webNumberPair(w io.Writer, field webField, value any, submitted url.Values) {
	decimal := webText(value)
	hex := webHexText(decimal, webHexWidth(field))
	base := "decimal"
	if submitted != nil {
		decimal = submitted.Get(field.Name)
		hex = submitted.Get(field.Name + "__hex")
		if submitted.Get(field.Name+"__base") == "hex" {
			base = "hex"
		}
	}
	fmt.Fprintf(w, `<div class=ts-numberpair data-hex-width=%d><input type=hidden data-number-source name="%s__base" value="%s"><label for="f-%s">Decimal<input id="f-%s" type=number step=1 data-number-decimal name="%s" value="%s"></label><label for="h-%s">Hexadecimal<input id="h-%s" type=text spellcheck=false data-number-hex data-hex-width=%d name="%s__hex" value="%s"></label></div>`, webHexWidth(field), esc(field.Name), base, esc(field.Name), esc(field.Name), esc(field.Name), esc(decimal), esc(field.Name), esc(field.Name), webHexWidth(field), esc(field.Name), esc(hex))
}
