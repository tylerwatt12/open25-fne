package framework

import "io"

// InitialConfig is the current configuration accepted when creating a new
// installation. Each document is subsequently owned by its respective service.
type InitialConfig struct {
	System        SystemConfig         `json:"system"`
	Zone          ZoneConfig           `json:"zone"`
	Sites         []SiteConfig         `json:"sites"`
	Application   ApplicationConfig    `json:"application"`
	Administrator InitialAdministrator `json:"administrator"`
}

type InitialAdministrator struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// DecodeInitialConfig accepts only current service documents. It performs no
// writes and never reads an earlier application's database or configuration.
func DecodeInitialConfig(reader io.Reader) (InitialConfig, error) {
	var initial InitialConfig
	raw, err := io.ReadAll(io.LimitReader(reader, maxConfigurationBytes+1))
	if err != nil {
		return initial, err
	}
	if err = decodeDocument(raw, &initial); err != nil {
		return InitialConfig{}, err
	}
	if err = validateInitialConfig(&initial); err != nil {
		return InitialConfig{}, err
	}
	return initial, nil
}

func validateInitialConfig(initial *InitialConfig) error {
	return validAdministrator(initial.Administrator.Username, []byte(initial.Administrator.PasswordHash))
}
