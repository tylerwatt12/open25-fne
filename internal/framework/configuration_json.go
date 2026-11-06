package framework

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const maxConfigurationBytes = 64 << 20

var recordID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func decodeDocument(raw json.RawMessage, target any) error {
	if len(raw) == 0 || len(raw) > maxConfigurationBytes {
		return errors.New("configuration must be a JSON object no larger than 64 MiB")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("configuration must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("configuration has trailing data")
	}
	return nil
}

// configurationJSON preserves entered settings without compatibility checks.
func configurationJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > maxConfigurationBytes {
		return nil, errors.New("configuration exceeds 64 MiB")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("configuration must be a JSON object")
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, err
	}
	return json.RawMessage(out.Bytes()), nil
}

func siteExists(z *ZoneConfig, id string) bool {
	for _, s := range z.Sites {
		if s.ID == id {
			return true
		}
	}
	return false
}
