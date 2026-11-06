// One-off preparation of current-format operational state before activation.
// This is not linked into the service, a runtime migration, or a radio action.
package main

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"open25.local/open25-fne/internal/framework"
)

func within(root, path string) bool {
	rel, e := filepath.Rel(root, path)
	return e == nil && rel != ".." && len(rel) > 0 && !(len(rel) > 3 && rel[:3] == "../")
}
func privateRead(path string) ([]byte, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("private state file must be an owner-only regular file belonging to this account")
	}
	return io.ReadAll(f)
}
func applicationKey(path string) (*ecdh.PrivateKey, error) {
	raw, e := privateRead(path)
	if errors.Is(e, os.ErrNotExist) {
		key, e := ecdh.X25519().GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return nil, e
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.Write(key.Bytes())
		closeErr := f.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return key, nil
	}
	if e != nil {
		return nil, e
	}
	defer clear(raw)
	return ecdh.X25519().NewPrivateKey(raw)
}
func signingKey(path string) (ed25519.PrivateKey, error) {
	seed, e := privateRead(path)
	if errors.Is(e, os.ErrNotExist) {
		seed = make([]byte, ed25519.SeedSize)
		if _, e = rand.Read(seed); e != nil {
			return nil, e
		}
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return nil, e
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.Write(seed)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if e != nil {
		return nil, e
	}
	defer clear(seed)
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("invalid System signing identity")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func run() error {
	rootFlag := flag.String("root", "", "current-format service state root")
	codec := flag.String("codec", "", "absolute packaged native dvmbridge launcher")
	apply := flag.Bool("apply", false, "write prepared current-format state")
	flag.Parse()
	if *rootFlag == "" || *codec == "" {
		return errors.New("use --root STATE_ROOT --codec /absolute/package/native/dvmbridge [--apply]")
	}
	root, e := filepath.Abs(*rootFlag)
	if e != nil {
		return e
	}
	if !filepath.IsAbs(*codec) {
		return errors.New("codec path must be absolute")
	}
	info, e := os.Stat(*codec)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("packaged codec executable is unavailable")
	}
	systemFile := filepath.Join(root, "system", "service.json")
	applicationFile := filepath.Join(root, "application", "service.json")
	system, e := framework.ReadFileConfig(systemFile)
	if e != nil {
		return e
	}
	endpoint, e := framework.ReadFileConfig(applicationFile)
	if e != nil {
		return e
	}
	if system.Role != framework.RoleSystem || endpoint.Role != framework.RoleApplication {
		return errors.New("state roles do not match")
	}
	for _, path := range []string{system.StateDir, system.CredentialFile, endpoint.StateDir, endpoint.CredentialFile} {
		if !within(root, path) {
			return errors.New("a service path points outside the supplied state root")
		}
	}
	store, e := framework.OpenStore(filepath.Join(system.StateDir, "configuration.db"), framework.RoleSystem)
	if e != nil {
		return e
	}
	defer store.Close()
	raw, e := store.Config()
	if e != nil {
		return e
	}
	var config framework.SystemConfig
	if e = framework.DecodeStrict(raw, &config); e != nil {
		return e
	}
	derived := 0
	if len(config.ConsolePositions) == 0 {
		groups := []int{}
		for _, group := range config.Talkgroups {
			if group.Enabled {
				groups = append(groups, group.TGID)
			}
		}
		for _, radio := range config.Radios {
			if radio.Enabled && radio.Kind == "console" {
				name := radio.Alias
				if name == "" {
					name = fmt.Sprintf("Console %d", radio.RID)
				}
				config.ConsolePositions = append(config.ConsolePositions, framework.ConsolePosition{ID: fmt.Sprintf("desk-%d", radio.RID), Name: name, RadioID: radio.RID, Enabled: true, Talkgroups: groups})
				derived++
			}
		}
	}
	// Serialize the programmed values; owner verifiers, radio identities,
	// Site settings and CWID settings are untouched.
	prepared, e := json.Marshal(config)
	if e != nil {
		return e
	}
	raw, e = privateRead(system.CredentialFile)
	if e != nil {
		return e
	}
	var credentials framework.ServiceCredentials
	if e = framework.DecodeStrict(raw, &credentials); e != nil {
		return e
	}
	clear(raw)
	raw, e = privateRead(endpoint.CredentialFile)
	if e != nil {
		return e
	}
	var endpointCredentials framework.ServiceCredentials
	if e = framework.DecodeStrict(raw, &endpointCredentials); e != nil {
		return e
	}
	clear(raw)
	fmt.Printf("Current-format preparation: %d new console positions; Application Server codec and crypto enrollment.\n", derived)
	if !*apply {
		fmt.Println("Preview only. Add --apply to write these settings. No radio operation is started.")
		return nil
	}
	signing, e := signingKey(filepath.Join(system.StateDir, "crypto", "system-signing.key"))
	if e != nil {
		return e
	}
	defer clear(signing)
	endpointCredentials.CryptoSystemPublicKey = hex.EncodeToString(signing.Public().(ed25519.PublicKey))
	key, e := applicationKey(filepath.Join(endpoint.StateDir, "crypto", "endpoint.key"))
	if e != nil {
		return e
	}
	if credentials.CryptoRecipients == nil {
		credentials.CryptoRecipients = map[string]string{}
	}
	credentials.CryptoRecipients[endpoint.InstanceID] = hex.EncodeToString(key.PublicKey().Bytes())
	if e = store.SaveConfig(prepared); e != nil {
		return e
	}
	if e = framework.WritePrivateJSON(system.CredentialFile, credentials); e != nil {
		return e
	}
	if e = framework.WritePrivateJSON(endpoint.CredentialFile, endpointCredentials); e != nil {
		return e
	}
	endpoint.Media.CodecBinary = *codec
	if e = framework.WritePrivateJSON(applicationFile, endpoint); e != nil {
		return e
	}
	fmt.Println("Preparation saved. Existing administrator verifiers and Site settings were preserved. No radio operation was started.")
	return nil
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "Preparation failed:", e)
		os.Exit(1)
	}
}
