package framework

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
)

// localInstanceConfigs reads the current shared-root layout. Enrollment needs
// paths and identities, not a compatibility check of saved operating settings.
func localInstanceConfigs(root string, role Role) ([]FileConfig, error) {
	paths := []string{filepath.Join(root, string(role), "service.json")}
	var directory string
	switch role {
	case RoleSite:
		directory = "sites"
	case RoleApplication:
		directory = "applications"
	}
	if directory != "" {
		extra, err := os.ReadDir(filepath.Join(root, directory))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, entry := range extra {
			if entry.IsDir() && entry.Name()[0] != '.' {
				paths = append(paths, filepath.Join(root, directory, entry.Name(), "service.json"))
			}
		}
	}
	configs := make([]FileConfig, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var cfg FileConfig
		if err = DecodeStrict(raw, &cfg); err != nil {
			return nil, err
		}
		if cfg.Role != role {
			return nil, fmt.Errorf("%s belongs to another service role", path)
		}
		configs = append(configs, cfg)
	}
	return configs, nil
}

// commitEnrollment adds only the new peer and makes the staged service visible.
// Restore earlier credential files if an ordinary write or final rename fails.
func commitEnrollment(stage, destination, identity string, peer PeerCredential, configs []FileConfig) error {
	type update struct {
		path   string
		before json.RawMessage
		after  ServiceCredentials
	}
	updates := make([]update, 0, len(configs))
	for _, cfg := range configs {
		creds, err := readCredentials(cfg.CredentialFile)
		if err != nil {
			return err
		}
		if _, exists := creds.Peers[identity]; exists {
			return fmt.Errorf("service credential %s already exists", identity)
		}
		before, err := os.ReadFile(cfg.CredentialFile)
		if err != nil {
			return err
		}
		if creds.Peers == nil {
			creds.Peers = map[string]PeerCredential{}
		}
		creds.Peers[identity] = peer
		updates = append(updates, update{cfg.CredentialFile, before, creds})
	}
	written := 0
	var err error
	for _, u := range updates {
		if err = WritePrivateJSON(u.path, u.after); err != nil {
			break
		}
		written++
	}
	if err == nil {
		err = os.Rename(stage, destination)
	}
	if err != nil {
		for i := written - 1; i >= 0; i-- {
			err = errors.Join(err, WritePrivateJSON(updates[i].path, updates[i].before))
		}
	}
	return err
}

// EnrollApplication creates a separate Application Server in an existing shared-root setup.
// Traffic-key authorization and the trusted System pin remain explicit owner
// operations in System Administration and the new Application Server's credentials.
func EnrollApplication(root, id, publicURL, listen, internalURL, internalListen string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if !recordID.MatchString(id) {
		return "", errors.New("invalid Application Server directory ID")
	}
	zoneFiles, err := localInstanceConfigs(root, RoleZone)
	if err != nil {
		return "", err
	}
	zoneFile := zoneFiles[0]
	zoneCreds, err := readCredentials(zoneFile.CredentialFile)
	if err != nil {
		return "", err
	}
	if zoneCreds.OutboundID == "" || len(zoneCreds.OutboundToken) < 32 {
		return "", errors.New("Zone outbound service credentials are not configured")
	}
	roleDir := filepath.Join(root, "applications", id)
	if _, err = os.Lstat(roleDir); err == nil {
		return "", errors.New("Application Server service files already exist")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	public, err := url.Parse(publicURL)
	if err != nil {
		return "", err
	}
	internal, err := url.Parse(internalURL)
	if err != nil {
		return "", err
	}
	if listen == "" {
		listen = public.Host
	}
	if internalListen == "" {
		internalListen = internal.Host
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	identity := "application:" + id
	creds := ServiceCredentials{OutboundID: identity, OutboundToken: token, Peers: map[string]PeerCredential{zoneCreds.OutboundID: {RoleZone, zoneFile.InstanceID, zoneCreds.OutboundToken}}}
	for key, peer := range zoneCreds.Peers {
		if peer.Role == RoleSite {
			creds.Peers[key] = peer
		}
	}
	cfg := FileConfig{Role: RoleApplication, InstanceID: id, Listen: listen, StateDir: filepath.Join(roleDir, "data"), PublicURL: publicURL, InternalURL: internalURL, InternalListen: internalListen, PollSeconds: zoneFile.PollSeconds, CredentialFile: filepath.Join(roleDir, "service-credentials.json"), TLSCertificate: zoneFile.TLSCertificate, TLSKey: zoneFile.TLSKey, AllowedCIDR: zoneFile.AllowedCIDR}
	listenHost, _, _ := net.SplitHostPort(internalListen)
	cfg.Media.VoiceListen = net.JoinHostPort(listenHost, "0")
	cfg.Media.VoiceAddress = net.JoinHostPort(internal.Hostname(), "0")
	if executable, executableErr := os.Executable(); executableErr == nil {
		cfg.Media.SpeechBinary = filepath.Join(filepath.Dir(executable), "speech", "espeak-ng")
	}
	parent := filepath.Dir(roleDir)
	if err = os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(parent, ".application-init-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if _, err = endpointKey(filepath.Join(stage, "data")); err != nil {
		return "", err
	}
	if err = WritePrivateJSON(filepath.Join(stage, "service.json"), cfg); err != nil {
		return "", err
	}
	if err = WritePrivateJSON(filepath.Join(stage, "service-credentials.json"), creds); err != nil {
		return "", err
	}
	zoneStore, err := OpenStore(filepath.Join(zoneFile.StateDir, "configuration.db"), RoleZone)
	if err != nil {
		return "", err
	}
	defer zoneStore.Close()
	store, err := OpenStore(filepath.Join(stage, "data", "configuration.db"), RoleApplication)
	if err != nil {
		return "", err
	}
	defer store.Close()
	for _, name := range []string{"system", "zone", "topology"} {
		raw, _, cacheErr := zoneStore.ReadCache(name)
		if cacheErr != nil {
			return "", cacheErr
		}
		if len(raw) != 0 {
			if err = store.WriteCache(name, raw); err != nil {
				return "", err
			}
		}
	}
	zoneRaw, err := zoneStore.Config()
	if err != nil {
		return "", err
	}
	if err = store.WriteCache("zone", zoneRaw); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(ApplicationConfig{Name: id, ConnectionURL: zoneFile.InternalURL})
	if err = store.SaveConfig(raw); err != nil {
		return "", err
	}
	username, verifier, err := zoneStore.Account()
	if err != nil {
		return "", err
	}
	if err = store.SetAccount(username, verifier); err != nil {
		return "", err
	}
	if err = store.Close(); err != nil {
		return "", err
	}
	siteFiles, err := localInstanceConfigs(root, RoleSite)
	if err != nil {
		return "", err
	}
	if err = commitEnrollment(stage, roleDir, identity, PeerCredential{RoleApplication, id, token}, append(zoneFiles, siteFiles...)); err != nil {
		return "", err
	}
	return filepath.Join(roleDir, "service.json"), nil
}
