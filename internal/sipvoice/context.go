package sipvoice

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
)

const contextType = "application/vnd.trunksystem.call-context+json"
const answerType = "application/vnd.trunksystem.call-answer+json"

type answerEnvelope struct {
	CallID  string          `json:"call_id"`
	Context json.RawMessage `json:"context"`
}

func validContext(p []byte) bool {
	return len(p) > 0 && len(p) <= 8192 && json.Valid(p) && bytes.HasPrefix(bytes.TrimSpace(p), []byte("{"))
}
func makeBody(c Call, raw []byte) (string, []byte, error) {
	if !c.valid() {
		return "", nil, ErrRejected
	}
	p, e := json.Marshal(c)
	if e != nil {
		return "", nil, ErrRejected
	}
	return packBody(raw, contextType, p)
}
func makeAnswerBody(id string, p json.RawMessage, raw []byte) (string, []byte, error) {
	if !token(id, 128) || !validContext(p) {
		return "", nil, ErrRejected
	}
	ctx, e := json.Marshal(answerEnvelope{id, p})
	if e != nil {
		return "", nil, ErrRejected
	}
	return packBody(raw, answerType, ctx)
}
func packBody(raw []byte, typ string, ctx []byte) (string, []byte, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, p := range []struct {
		typ  string
		data []byte
	}{{"application/sdp", raw}, {typ, ctx}} {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", p.typ)
		part, e := w.CreatePart(h)
		if e != nil {
			return "", nil, ErrRejected
		}
		if _, e = part.Write(p.data); e != nil {
			return "", nil, ErrRejected
		}
	}
	if w.Close() != nil || b.Len() > maxBody {
		return "", nil, ErrRejected
	}
	return "multipart/mixed; boundary=" + w.Boundary(), b.Bytes(), nil
}
func parseBody(ct string, body []byte) (Call, []byte, error) {
	var c Call
	raw, p, e := unpackBody(ct, body, contextType)
	if e != nil {
		return c, nil, e
	}
	if decodeContext(p, &c) != nil || !c.valid() {
		return Call{}, nil, ErrRejected
	}
	return c, raw, nil
}
func parseAnswerBody(ct string, body []byte, id string) (json.RawMessage, []byte, error) {
	var a answerEnvelope
	raw, p, e := unpackBody(ct, body, answerType)
	if e != nil {
		return nil, nil, e
	}
	if decodeContext(p, &a) != nil || a.CallID != id || !validContext(a.Context) {
		return nil, nil, ErrRejected
	}
	return bytes.Clone(a.Context), raw, nil
}
func decodeContext(p []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(p))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil || d.Decode(&struct{}{}) != io.EOF {
		return ErrRejected
	}
	return nil
}
func unpackBody(ct string, body []byte, want string) (raw, ctx []byte, err error) {
	if len(body) > maxBody {
		return nil, nil, ErrRejected
	}
	typ, p, e := mime.ParseMediaType(ct)
	if e != nil || typ != "multipart/mixed" || len(p["boundary"]) > 70 || p["boundary"] == "" {
		return nil, nil, ErrRejected
	}
	r := multipart.NewReader(bytes.NewReader(body), p["boundary"])
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		part, e := r.NextPart()
		if e != nil {
			return nil, nil, ErrRejected
		}
		typ := part.Header.Get("Content-Type")
		if seen[typ] {
			return nil, nil, ErrRejected
		}
		seen[typ] = true
		data, e := io.ReadAll(io.LimitReader(part, maxBody+1))
		if e != nil || len(data) > maxBody {
			return nil, nil, ErrRejected
		}
		switch typ {
		case "application/sdp":
			raw = data
		case want:
			ctx = data
		default:
			return nil, nil, ErrRejected
		}
	}
	if _, e = r.NextPart(); e != io.EOF || raw == nil || ctx == nil {
		return nil, nil, ErrRejected
	}
	return raw, ctx, nil
}
