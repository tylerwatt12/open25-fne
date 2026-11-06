package framework

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileConfig selects one independent server and its local resources.
type FileConfig struct {
	Role           Role                `json:"role"`
	InstanceID     string              `json:"instance_id"`
	Listen         string              `json:"listen"`
	StateDir       string              `json:"state_dir"`
	PublicURL      string              `json:"public_url"`
	InternalListen string              `json:"internal_listen"`
	InternalURL    string              `json:"internal_url"`
	UpstreamURL    string              `json:"upstream_url"`
	PollSeconds    int                 `json:"poll_seconds"`
	CredentialFile string              `json:"credential_file"`
	TLSCertificate string              `json:"tls_certificate"`
	TLSKey         string              `json:"tls_key"`
	AllowedCIDR    string              `json:"allowed_cidr"`
	Radio          RadioRuntimeOptions `json:"radio"`
	Media          MediaOptions        `json:"media"`
}
type RadioRuntimeOptions struct {
	AllowRF     bool   `json:"allow_rf"`
	Engine      string `json:"engine"`
	FNE         string `json:"fne"`
	Bridge      string `json:"bridge"`
	BaseRPCPort int    `json:"base_rpc_port"`
	FNEPort     int    `json:"fne_port"`
}
type MediaOptions struct {
	VoiceListen  string `json:"voice_listen"`
	VoiceAddress string `json:"voice_address"`
	ICEPortMin   uint16 `json:"ice_port_min"`
	ICEPortMax   uint16 `json:"ice_port_max"`
	CodecBinary  string `json:"codec_binary"`
	SpeechBinary string `json:"speech_binary"`
}

type PeerCredential struct {
	Role       Role   `json:"role"`
	InstanceID string `json:"instance_id"`
	Token      string `json:"token"`
}

type ServiceCredentials struct {
	OutboundID            string                    `json:"outbound_id"`
	OutboundToken         string                    `json:"outbound_token"`
	Peers                 map[string]PeerCredential `json:"peers"`
	CryptoRecipients      map[string]string         `json:"crypto_recipients"`
	CryptoSystemPublicKey string                    `json:"crypto_system_public_key"`
}

type Settings struct {
	UpstreamURL string `json:"upstream_url"`
	PublicURL   string `json:"public_url"`
	InternalURL string `json:"internal_url"`
	PollSeconds int    `json:"poll_seconds"`
}

func (c FileConfig) Settings() Settings {
	return Settings{UpstreamURL: c.UpstreamURL, PublicURL: c.PublicURL, InternalURL: c.InternalURL, PollSeconds: c.PollSeconds}
}

func DecodeStrict(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("provide one JSON object")
	}
	return nil
}

func validateURL(value string, optional bool) error {
	if value == "" && optional {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("enter a server address such as https://192.0.2.89:5001, without a path or password")
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return errors.New("use HTTPS for a server outside this computer")
	}
	return nil
}

// Internal origins are explicit plain HTTP endpoints on the deployment VPN.
// They are never browser navigation addresses or HTTPS downgrade candidates.
func validateInternalURL(value string, optional bool) error {
	if value == "" && optional {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return errors.New("enter an internal HTTP server address such as http://198.51.100.2:5101, without a path or password")
	}
	return nil
}

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

func (c FileConfig) Validate() error {
	if !c.Role.Valid() || strings.TrimSpace(c.InstanceID) == "" {
		return errors.New("role and instance_id are required")
	}
	if c.Role != RoleSite && (c.Radio.AllowRF || c.Radio.Engine != "" || c.Radio.FNE != "" || c.Radio.Bridge != "") {
		return errors.New("radio engines are configured only at a Site")
	}
	if c.Radio.AllowRF && (c.Radio.Engine == "" || c.Radio.FNE == "" || c.Radio.Bridge == "") {
		return errors.New("RF operation needs the packaged radio engine, voice network and audio bridge")
	}
	if (c.Media.ICEPortMin == 0) != (c.Media.ICEPortMax == 0) || c.Media.ICEPortMax < c.Media.ICEPortMin {
		return errors.New("invalid media port range")
	}
	if (c.Media.VoiceListen == "") != (c.Media.VoiceAddress == "") {
		return errors.New("configure both voice_listen and voice_address")
	}
	if c.Media.VoiceListen != "" {
		if c.Role == RoleSystem {
			return errors.New("System Administration does not carry radio voice")
		}
		if _, _, err := net.SplitHostPort(c.Media.VoiceListen); err != nil {
			return errors.New("invalid voice listener")
		}
		host, port, err := net.SplitHostPort(c.Media.VoiceAddress)
		if err != nil || host == "" || port == "" {
			return errors.New("voice_address needs an explicit host and port")
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsMulticast() || !ip.To4().Equal(ip)) {
			return errors.New("voice_address needs a unicast IPv4 host")
		}
	}
	if c.StateDir == "" || c.CredentialFile == "" {
		return errors.New("state_dir and credential_file are required")
	}
	if c.PollSeconds < 1 || c.PollSeconds > 300 {
		return errors.New("poll_seconds must be between 1 and 300")
	}
	if err := validateURL(c.PublicURL, false); err != nil {
		return err
	}
	if err := validateInternalURL(c.InternalURL, false); err != nil {
		return err
	}
	if err := validateInternalURL(c.UpstreamURL, c.Role == RoleSystem || c.Role == RoleApplication); err != nil {
		return err
	}
	if (c.Role == RoleSystem || c.Role == RoleApplication) && c.UpstreamURL != "" {
		return errors.New("this role does not use upstream_url in the service file; Application Server owns its connection in its saved configuration")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if _, _, err := net.SplitHostPort(c.InternalListen); err != nil {
		return fmt.Errorf("internal_listen: %w", err)
	}
	if c.InternalListen == c.Listen && !strings.HasSuffix(c.Listen, ":0") {
		return errors.New("web and internal listeners need different addresses")
	}
	if (c.TLSCertificate == "") != (c.TLSKey == "") {
		return errors.New("provide both TLS certificate and key")
	}
	if c.TLSCertificate == "" && !loopbackHost(host) {
		return errors.New("a network listener requires HTTPS; use a loopback address for local HTTP")
	}
	web, _ := url.Parse(c.PublicURL)
	if (web.Scheme == "https") != (c.TLSCertificate != "") {
		return errors.New("administration address and web TLS settings must use the same scheme")
	}
	if c.AllowedCIDR != "" {
		if _, _, err := net.ParseCIDR(c.AllowedCIDR); err != nil {
			return errors.New("allowed_cidr is invalid")
		}
	}
	return nil
}

func ReadFileConfig(path string) (FileConfig, error) {
	var c FileConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = DecodeStrict(raw, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func WritePrivateJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".configuration-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readCredentials(path string) (ServiceCredentials, error) {
	var c ServiceCredentials
	info, err := os.Stat(path)
	if err != nil {
		return c, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return c, errors.New("credential file must be readable only by its owner")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = DecodeStrict(raw, &c); err != nil {
		return c, err
	}
	for id, p := range c.Peers {
		if id == "" || !p.Role.Valid() || p.InstanceID == "" || len(p.Token) < 32 {
			return c, errors.New("invalid service credential")
		}
	}
	return c, nil
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("upstream redirects are not allowed") }}
}
