package framework

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type InitOptions struct {
	Root                   string
	ListenHost             string
	InternalListenHost     string
	SystemInternalURL      string
	ZoneInternalURL        string
	SiteInternalURL        string
	ApplicationInternalURL string
	SystemURL              string
	ZoneURL                string
	SiteURL                string
	ApplicationURL         string
	TLSCertificate         string
	TLSKey                 string
	AllowedCIDR            string
}

type InitReceipt struct {
	At                 time.Time       `json:"at"`
	Administrator      string          `json:"administrator"`
	ConfigurationFiles map[Role]string `json:"configuration_files"`
	URLs               map[Role]string `json:"urls"`
	InternalURLs       map[Role]string `json:"internal_urls"`
	SystemID           string          `json:"system_record"`
	ZoneID             string          `json:"zone_record"`
	SiteID             string          `json:"site_record"`
	RFEnabled          bool            `json:"rf_enabled"`
}

// Initialize creates independent services from current-format configuration.
// It never attaches a modem or overwrites an existing installation.
func Initialize(seed InitialConfig, o InitOptions) (InitReceipt, error) {
	if err := validateInitialConfig(&seed); err != nil {
		return InitReceipt{}, err
	}
	receipt := InitReceipt{At: time.Now().UTC(), Administrator: seed.Administrator.Username, ConfigurationFiles: map[Role]string{}, URLs: map[Role]string{}, InternalURLs: map[Role]string{}}
	if o.Root == "" {
		return receipt, errors.New("root directory is required")
	}
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return receipt, err
	}
	if _, err = os.Lstat(root); err == nil {
		return receipt, errors.New("framework root already exists; choose a new empty location")
	} else if !errors.Is(err, os.ErrNotExist) {
		return receipt, err
	}
	if len(seed.Sites) != 1 {
		return receipt, errors.New("initialization creates one Site service; add further Site records in Zone Administration")
	}
	if o.ListenHost == "" {
		o.ListenHost = "127.0.0.1"
	}
	if o.InternalListenHost == "" {
		o.InternalListenHost = "127.0.0.1"
	}
	internalURLs := map[Role]string{RoleSystem: o.SystemInternalURL, RoleZone: o.ZoneInternalURL, RoleSite: o.SiteInternalURL, RoleApplication: o.ApplicationInternalURL}
	internalPorts := map[Role]string{RoleSystem: "5100", RoleZone: "5101", RoleSite: "5102", RoleApplication: "5103"}
	urls := map[Role]string{RoleSystem: o.SystemURL, RoleZone: o.ZoneURL, RoleSite: o.SiteURL, RoleApplication: o.ApplicationURL}
	ports := map[Role]string{RoleSystem: "5000", RoleZone: "5001", RoleSite: "5002", RoleApplication: "5003"}
	for _, role := range []Role{RoleSystem, RoleZone, RoleSite, RoleApplication} {
		if urls[role] == "" {
			urls[role] = "http://127.0.0.1:" + ports[role]
		}
		if err = validateURL(urls[role], false); err != nil {
			return receipt, err
		}
		if internalURLs[role] == "" {
			internalURLs[role] = "http://127.0.0.1:" + internalPorts[role]
		}
		if err = validateInternalURL(internalURLs[role], false); err != nil {
			return receipt, err
		}
		address, _ := url.Parse(internalURLs[role])
		internalPorts[role] = address.Port()
		if internalPorts[role] == "" {
			internalPorts[role] = "80"
		}
		address, _ = url.Parse(urls[role])
		if address.Port() != "" {
			ports[role] = address.Port()
		}
	}
	siteID := seed.Sites[0].ID
	for i := range seed.Zone.Sites {
		if seed.Zone.Sites[i].ID == siteID {
			seed.Zone.Sites[i].Endpoint = internalURLs[RoleSite]
			seed.Zone.Sites[i].AdminURL = urls[RoleSite]
		}
	}
	seed.Application.ConnectionURL = internalURLs[RoleZone]
	if seed.Application.LocalSiteURL != "" {
		seed.Application.LocalSiteURL = internalURLs[RoleSite]
	}
	ids := map[Role]string{RoleSystem: seed.System.ID, RoleZone: seed.Zone.ID, RoleSite: siteID, RoleApplication: "application-1"}
	zoneToken, err := randomToken()
	if err != nil {
		return receipt, err
	}
	siteToken, err := randomToken()
	if err != nil {
		return receipt, err
	}
	applicationToken, err := randomToken()
	if err != nil {
		return receipt, err
	}
	zoneKey := "zone:" + seed.Zone.ID
	siteKey := "site:" + siteID
	applicationKey := "application:application-1"
	zonePeer := PeerCredential{RoleZone, seed.Zone.ID, zoneToken}
	sitePeer := PeerCredential{RoleSite, siteID, siteToken}
	applicationPeer := PeerCredential{RoleApplication, "application-1", applicationToken}
	credentials := map[Role]ServiceCredentials{
		RoleSystem:      {Peers: map[string]PeerCredential{zoneKey: zonePeer}},
		RoleZone:        {OutboundID: zoneKey, OutboundToken: zoneToken, Peers: map[string]PeerCredential{siteKey: sitePeer, applicationKey: applicationPeer}},
		RoleSite:        {OutboundID: siteKey, OutboundToken: siteToken, Peers: map[string]PeerCredential{zoneKey: zonePeer, applicationKey: applicationPeer}},
		RoleApplication: {OutboundID: applicationKey, OutboundToken: applicationToken, Peers: map[string]PeerCredential{zoneKey: zonePeer, siteKey: sitePeer}},
	}
	configs := map[Role]any{RoleSystem: seed.System, RoleZone: seed.Zone, RoleSite: seed.Sites[0], RoleApplication: seed.Application}
	parent := filepath.Dir(root)
	if err = os.MkdirAll(parent, 0700); err != nil {
		return receipt, err
	}
	stage, err := os.MkdirTemp(parent, ".framework-init-*")
	if err != nil {
		return receipt, err
	}
	defer os.RemoveAll(stage)
	// Application Server's private identity is generated in staged data and survives the
	// atomic move. System Administration receives only its public recipient key.
	applicationIdentity, err := endpointKey(filepath.Join(stage, string(RoleApplication), "data"))
	if err != nil {
		return receipt, errors.New("could not initialize Application Server crypto identity")
	}
	systemCredentials := credentials[RoleSystem]
	systemCredentials.CryptoRecipients = map[string]string{ids[RoleApplication]: hex.EncodeToString(applicationIdentity.PublicKey().Bytes())}
	credentials[RoleSystem] = systemCredentials
	signing, err := systemSigningKey(filepath.Join(stage, string(RoleSystem), "data"))
	if err != nil {
		return receipt, errors.New("could not initialize System signing identity")
	}
	defer clear(signing)
	applicationCredentials := credentials[RoleApplication]
	applicationCredentials.CryptoSystemPublicKey = hex.EncodeToString(signing.Public().(ed25519.PublicKey))
	credentials[RoleApplication] = applicationCredentials
	tree := topology{SystemURL: urls[RoleSystem], ZoneURL: urls[RoleZone], ApplicationURL: urls[RoleApplication], SiteURLs: map[string]string{siteID: urls[RoleSite]}}
	for _, role := range []Role{RoleSystem, RoleZone, RoleSite, RoleApplication} {
		roleDir := filepath.Join(root, string(role))
		stageRole := filepath.Join(stage, string(role))
		cfg := FileConfig{Role: role, InstanceID: ids[role], Listen: net.JoinHostPort(o.ListenHost, ports[role]), StateDir: filepath.Join(roleDir, "data"), PublicURL: urls[role], InternalListen: net.JoinHostPort(o.InternalListenHost, internalPorts[role]), InternalURL: internalURLs[role], PollSeconds: 2, CredentialFile: filepath.Join(roleDir, "service-credentials.json"), TLSCertificate: o.TLSCertificate, TLSKey: o.TLSKey, AllowedCIDR: o.AllowedCIDR}
		if role != RoleSystem {
			u, _ := url.Parse(internalURLs[role])
			cfg.Media.VoiceListen = net.JoinHostPort(o.InternalListenHost, "0")
			cfg.Media.VoiceAddress = net.JoinHostPort(u.Hostname(), "0")
		}
		if role == RoleApplication {
			if executable, executableErr := os.Executable(); executableErr == nil {
				cfg.Media.SpeechBinary = filepath.Join(filepath.Dir(executable), "speech", "espeak-ng")
			}
		}
		if role == RoleZone {
			cfg.UpstreamURL = internalURLs[RoleSystem]
		}
		if role == RoleSite {
			cfg.UpstreamURL = internalURLs[RoleZone]
		}
		if err = cfg.Validate(); err != nil {
			return receipt, fmt.Errorf("%s: %w", role, err)
		}
		if err = WritePrivateJSON(filepath.Join(stageRole, "service.json"), cfg); err != nil {
			return receipt, err
		}
		if err = WritePrivateJSON(filepath.Join(stageRole, "service-credentials.json"), credentials[role]); err != nil {
			return receipt, err
		}
		store, err := OpenStore(filepath.Join(stageRole, "data", "configuration.db"), role)
		if err != nil {
			return receipt, err
		}
		err = seedStore(store, seed, configs[role], tree)
		closeErr := store.Close()
		if err != nil {
			return receipt, fmt.Errorf("%s: %w", role, err)
		}
		if closeErr != nil {
			return receipt, closeErr
		}
		if role == RoleZone {
			err = writeRFAuthority(filepath.Join(stageRole, "data", "rf-source-authority.json"), rfAuthorityDocument{ZoneID: seed.Zone.ID, NativeRuns: map[string]time.Time{}, Units: map[int]rfAuthorityUnit{}})
			if err != nil {
				return receipt, err
			}
		}
		receipt.ConfigurationFiles[role] = filepath.Join(roleDir, "service.json")
		receipt.URLs[role] = urls[role]
		receipt.InternalURLs[role] = internalURLs[role]
	}
	receipt.SystemID = seed.System.ID
	receipt.ZoneID = seed.Zone.ID
	receipt.SiteID = siteID
	if err = WritePrivateJSON(filepath.Join(stage, "initialization-receipt.json"), receipt); err != nil {
		return receipt, err
	}
	// Do not let a second initializer replace a directory created during initialization.
	if _, err = os.Lstat(root); err == nil {
		return receipt, errors.New("framework root appeared during initialization; it was left unchanged")
	}
	if err = os.Rename(stage, root); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func seedStore(store *Store, seed InitialConfig, config any, tree topology) error {
	for name, value := range map[string]any{"system": seed.System, "zone": seed.Zone, "topology": tree} {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err = store.WriteCache(name, raw); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if err = store.SaveConfig(raw); err != nil {
		return err
	}
	if err = store.SetAccount(seed.Administrator.Username, []byte(seed.Administrator.PasswordHash)); err != nil {
		return err
	}
	return store.AddEvent("initialization", "Service configuration initialized. RF operation is stopped.")
}

// EnrollSite creates only the new Site's service files and its Zone credential.
// The Site record must already have been created in Zone Administration.
func EnrollSite(root, id, publicURL, listen, internalURL, internalListen string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	zoneFile, err := ReadFileConfig(filepath.Join(root, "zone", "service.json"))
	if err != nil {
		return "", err
	}
	zoneStore, err := OpenStore(filepath.Join(zoneFile.StateDir, "configuration.db"), RoleZone)
	if err != nil {
		return "", err
	}
	defer zoneStore.Close()
	zoneRaw, err := zoneStore.Config()
	if err != nil {
		return "", err
	}
	var zone ZoneConfig
	if err = json.Unmarshal(zoneRaw, &zone); err != nil {
		return "", err
	}
	found := false
	for _, site := range zone.Sites {
		if site.ID == id {
			found = true
			break
		}
	}
	if !found {
		return "", errors.New("create the Site record in Zone Administration first")
	}
	if err = validateURL(publicURL, false); err != nil {
		return "", err
	}
	if err = validateInternalURL(internalURL, false); err != nil {
		return "", err
	}
	if !recordID.MatchString(id) {
		return "", errors.New("invalid Site record ID")
	}
	roleDir := filepath.Join(root, "sites", id)
	if _, err = os.Lstat(roleDir); err == nil {
		return "", errors.New("Site service files already exist")
	}
	creds, err := readCredentials(zoneFile.CredentialFile)
	if err != nil {
		return "", err
	}
	if creds.OutboundID == "" || len(creds.OutboundToken) < 32 {
		return "", errors.New("Zone outbound service credentials are not configured")
	}
	key := "site:" + id
	if _, ok := creds.Peers[key]; ok {
		return "", errors.New("Site credentials already exist")
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	siteCreds := ServiceCredentials{OutboundID: key, OutboundToken: token, Peers: map[string]PeerCredential{creds.OutboundID: {RoleZone, zoneFile.InstanceID, creds.OutboundToken}}}
	for k, p := range creds.Peers {
		if p.Role == RoleApplication {
			siteCreds.Peers[k] = p
		}
	}
	if listen == "" {
		u, _ := url.Parse(publicURL)
		listen = u.Host
		if _, portErr := net.ResolveTCPAddr("tcp", listen); portErr != nil {
			return "", errors.New("specify --listen with a host and port")
		}
	}
	if internalListen == "" {
		u, _ := url.Parse(internalURL)
		internalListen = u.Host
		if _, portErr := net.ResolveTCPAddr("tcp", internalListen); portErr != nil {
			return "", errors.New("specify --internal-listen with a host and port")
		}
	}
	cfg := FileConfig{Role: RoleSite, InstanceID: id, Listen: listen, StateDir: filepath.Join(roleDir, "data"), PublicURL: publicURL, InternalURL: internalURL, InternalListen: internalListen, UpstreamURL: zoneFile.InternalURL, PollSeconds: zoneFile.PollSeconds, CredentialFile: filepath.Join(roleDir, "service-credentials.json"), TLSCertificate: zoneFile.TLSCertificate, TLSKey: zoneFile.TLSKey, AllowedCIDR: zoneFile.AllowedCIDR}
	listenHost, _, _ := net.SplitHostPort(internalListen)
	advertised, _ := url.Parse(internalURL)
	cfg.Media.VoiceListen = net.JoinHostPort(listenHost, "0")
	cfg.Media.VoiceAddress = net.JoinHostPort(advertised.Hostname(), "0")
	if err = cfg.Validate(); err != nil {
		return "", err
	}
	parent := filepath.Dir(roleDir)
	if err = os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(parent, ".site-init-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err = WritePrivateJSON(filepath.Join(stage, "service.json"), cfg); err != nil {
		return "", err
	}
	if err = WritePrivateJSON(filepath.Join(stage, "service-credentials.json"), siteCreds); err != nil {
		return "", err
	}
	store, err := OpenStore(filepath.Join(stage, "data", "configuration.db"), RoleSite)
	if err != nil {
		return "", err
	}
	defer store.Close()
	systemRaw, _, err := zoneStore.ReadCache("system")
	if err != nil {
		return "", err
	}
	if err = store.WriteCache("system", systemRaw); err != nil {
		return "", err
	}
	if err = store.WriteCache("zone", zoneRaw); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(SiteConfig{ID: id, Modems: []Modem{}, Channels: []Channel{}, Identification: Identification{Enabled: true, Mode: "ham", WPM: 20, QuietSeconds: 60}, ControlPolicy: ControlPolicy{Mode: "static", RotationHours: 24}})
	if err = store.SaveConfig(raw); err != nil {
		return "", err
	}
	username, hash, err := zoneStore.Account()
	if err != nil {
		return "", err
	}
	if err = store.SetAccount(username, hash); err != nil {
		return "", err
	}
	if err = store.Close(); err != nil {
		return "", err
	}
	// Current SIP peers authenticate both directions, including Site fallback.
	applicationFiles, err := localInstanceConfigs(root, RoleApplication)
	if err != nil {
		return "", err
	}
	if err = commitEnrollment(stage, roleDir, key, PeerCredential{RoleSite, id, token}, append([]FileConfig{zoneFile}, applicationFiles...)); err != nil {
		return "", err
	}
	return filepath.Join(roleDir, "service.json"), nil
}
