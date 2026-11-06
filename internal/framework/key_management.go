package framework

import (
	"cmp"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/hkdf"
	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/dispatch"
	"open25.local/open25-fne/internal/p25crypto"
)

type trafficKey struct {
	AlgorithmID int    `json:"algorithm_id"`
	KeyID       int    `json:"key_id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Material    string `json:"material,omitempty"`
}
type keyEnvelope struct {
	SystemID   string `json:"system_id"`
	Signature  []byte `json:"signature,omitempty"`
	Recipient  string `json:"recipient"`
	Public     []byte `json:"public"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}
type protectedKeys struct {
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

func privateBytes(path string, size int, create bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		defer f.Close()
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("crypto files must be private regular files")
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
			return nil, errors.New("crypto files must belong to this service account")
		}
		b, e := io.ReadAll(io.LimitReader(f, int64(size+1)))
		if e != nil || len(b) != size {
			clear(b)
			return nil, errors.New("invalid private crypto file")
		}
		return b, nil
	}
	if !create || !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	b := make([]byte, size)
	if _, err = rand.Read(b); err != nil {
		return nil, err
	}
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		clear(b)
		return privateBytes(path, size, false)
	}
	if err != nil {
		clear(b)
		return nil, err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		clear(b)
		return nil, err
	}
	if closeErr != nil {
		clear(b)
		return nil, closeErr
	}
	return b, nil
}
func writeRuntimeKeys(path string, text []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".codec-keys-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(text); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func gcmFor(key []byte) (cipher.AEAD, error) {
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(b)
}
func (s *Service) readTrafficKeys() ([]trafficKey, error) {
	if s.Config().Role != RoleSystem {
		return nil, errors.New("traffic-key custody belongs to System Administration")
	}
	raw, _, err := s.Store.ReadCache("traffic_keys")
	if errors.Is(err, sql.ErrNoRows) {
		return []trafficKey{}, nil
	}
	if err != nil {
		return nil, err
	}
	var sealed protectedKeys
	if err = DecodeStrict(raw, &sealed); err != nil {
		return nil, errors.New("invalid protected key store")
	}
	master, e := privateBytes(filepath.Join(s.Config().StateDir, "crypto", "master.key"), 32, false)
	if e != nil {
		return nil, e
	}
	defer clear(master)
	a, e := gcmFor(master)
	if e != nil {
		return nil, e
	}
	if len(sealed.Nonce) != a.NonceSize() {
		return nil, errors.New("invalid protected key store")
	}
	plain, e := a.Open(nil, sealed.Nonce, sealed.Ciphertext, []byte(s.Config().InstanceID))
	if e != nil {
		return nil, errors.New("could not unlock the traffic-key store")
	}
	var keys []trafficKey
	e = DecodeStrict(plain, &keys)
	clear(plain)
	if e != nil {
		return nil, e
	}
	return keys, nil
}
func (s *Service) writeTrafficKeys(keys []trafficKey) error {
	if s.Config().Role != RoleSystem {
		return errors.New("traffic-key custody belongs to System Administration")
	}
	master, e := privateBytes(filepath.Join(s.Config().StateDir, "crypto", "master.key"), 32, true)
	if e != nil {
		return e
	}
	defer clear(master)
	a, e := gcmFor(master)
	if e != nil {
		return e
	}
	nonce := make([]byte, a.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return e
	}
	plain, e := json.Marshal(keys)
	if e != nil {
		return e
	}
	defer clear(plain)
	return s.writeCache("traffic_keys", protectedKeys{nonce, a.Seal(nil, nonce, plain, []byte(s.Config().InstanceID))})
}
func endpointKey(stateDir string) (*ecdh.PrivateKey, error) {
	b, e := privateBytes(filepath.Join(stateDir, "crypto", "endpoint.key"), 32, true)
	if e != nil {
		return nil, e
	}
	defer clear(b)
	return ecdh.X25519().NewPrivateKey(b)
}
func systemSigningKey(stateDir string) (ed25519.PrivateKey, error) {
	seed, e := privateBytes(filepath.Join(stateDir, "crypto", "system-signing.key"), ed25519.SeedSize, true)
	if e != nil {
		return nil, e
	}
	defer clear(seed)
	return ed25519.NewKeyFromSeed(seed), nil
}
func envelopeMessage(env keyEnvelope) []byte {
	env.Signature = nil
	raw, _ := json.Marshal(env)
	return raw
}

func envelopeAEAD(private *ecdh.PrivateKey, public []byte, aad string) (cipher.AEAD, error) {
	p, e := ecdh.X25519().NewPublicKey(public)
	if e != nil {
		return nil, e
	}
	shared, e := private.ECDH(p)
	if e != nil {
		return nil, e
	}
	defer clear(shared)
	key := make([]byte, 32)
	if _, e = io.ReadFull(hkdf.New(sha256.New, shared, nil, []byte(aad)), key); e != nil {
		return nil, e
	}
	defer clear(key)
	return gcmFor(key)
}
func (s *Service) keyEnvelopes() (map[string]keyEnvelope, error) {
	out := map[string]keyEnvelope{}
	if s.Config().Role != RoleSystem {
		raw, _, e := s.Store.ReadCache("key_envelopes")
		if errors.Is(e, sql.ErrNoRows) {
			return out, nil
		}
		if e != nil {
			return nil, e
		}
		if e = DecodeStrict(raw, &out); e != nil || out == nil {
			return nil, errors.New("invalid cached Application Server key envelopes")
		}
		for id, env := range out {
			if !recordID.MatchString(id) || env.Recipient != id || len(env.Public) != 32 || len(env.Nonce) != 12 || len(env.Ciphertext) < 16 || len(env.Signature) != ed25519.SignatureSize || env.SystemID == "" {
				return nil, errors.New("invalid cached Application Server key envelope")
			}
		}
		return out, nil
	}
	s.featureMu.Lock()
	defer s.featureMu.Unlock()
	keys, e := s.readTrafficKeys()
	if e != nil {
		return nil, e
	}
	creds, e := readCredentials(s.Config().CredentialFile)
	if e != nil {
		return nil, e
	}
	plain, e := json.Marshal(keys)
	if e != nil {
		return nil, e
	}
	defer clear(plain)
	systemID := s.view().System.ID
	signing, e := systemSigningKey(s.Config().StateDir)
	if e != nil {
		return nil, e
	}
	defer clear(signing)
	for recipient, raw := range creds.CryptoRecipients {
		pub, e := hex.DecodeString(raw)
		if !recordID.MatchString(recipient) || e != nil || len(pub) != 32 {
			return nil, errors.New("invalid Application Server crypto enrollment")
		}
		ephemeral, e := ecdh.X25519().GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		aad := systemID + "/" + recipient
		a, e := envelopeAEAD(ephemeral, pub, aad)
		if e != nil {
			return nil, errors.New("invalid Application Server crypto enrollment")
		}
		nonce := make([]byte, a.NonceSize())
		if _, e = rand.Read(nonce); e != nil {
			return nil, e
		}
		env := keyEnvelope{SystemID: systemID, Recipient: recipient, Public: ephemeral.PublicKey().Bytes(), Nonce: nonce, Ciphertext: a.Seal(nil, nonce, plain, []byte(aad))}
		env.Signature = ed25519.Sign(signing, envelopeMessage(env))
		out[recipient] = env
	}
	return out, nil
}
func openKeyEnvelope(private *ecdh.PrivateKey, systemID, recipient string, env keyEnvelope, systemPublic ed25519.PublicKey) ([]trafficKey, error) {
	if env.Recipient != recipient {
		return nil, errors.New("Application Server key envelope recipient does not match")
	}
	if env.SystemID != systemID || len(systemPublic) != ed25519.PublicKeySize || !ed25519.Verify(systemPublic, envelopeMessage(env), env.Signature) {
		return nil, errors.New("Application Server key inventory is not signed by the pinned System authority")
	}

	aad := systemID + "/" + recipient
	a, e := envelopeAEAD(private, env.Public, aad)
	if e != nil {
		return nil, errors.New("Application Server key envelope is invalid")
	}
	if len(env.Nonce) != a.NonceSize() {
		return nil, errors.New("Application Server key envelope nonce is invalid")
	}
	plain, e := a.Open(nil, env.Nonce, env.Ciphertext, []byte(aad))
	if e != nil {
		return nil, errors.New("Application Server key envelope could not be opened")
	}
	defer clear(plain)
	var keys []trafficKey
	if e = DecodeStrict(plain, &keys); e != nil {
		return nil, errors.New("Application Server key envelope contents are invalid")
	}
	return keys, nil
}

func (s *Service) cryptoHTTP(w http.ResponseWriter, r *http.Request, session Session) {
	if session.UserID != "" {
		fail(w, 403, errors.New("crypto administration requires administrator sign-in"))
		return
	}
	role := s.Config().Role
	if r.URL.Path == "/api/crypto/identity" && role == RoleSystem && r.Method == http.MethodGet {
		signing, e := systemSigningKey(s.Config().StateDir)
		if e != nil {
			fail(w, 500, errors.New("could not open System signing identity"))
			return
		}
		defer clear(signing)
		respond(w, 200, map[string]any{"system_id": s.view().System.ID, "public_key": hex.EncodeToString(signing.Public().(ed25519.PublicKey))})
		return
	}
	if r.URL.Path == "/api/crypto/identity" && role == RoleApplication && r.Method == "GET" {
		key, e := endpointKey(s.Config().StateDir)
		if e != nil {
			fail(w, 500, errors.New("could not open Application Server crypto identity"))
			return
		}
		respond(w, 200, map[string]any{"recipient": s.Config().InstanceID, "public_key": hex.EncodeToString(key.PublicKey().Bytes())})
		return
	}
	if role != RoleSystem {
		fail(w, 403, errors.New("traffic keys are managed in System Administration"))
		return
	}
	s.featureMu.Lock()
	defer s.featureMu.Unlock()
	kind, resource, selected := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/crypto/"), "/")
	if kind != "keys" && kind != "recipients" {
		http.NotFound(w, r)
		return
	}
	if (!selected && r.Method != http.MethodGet && r.Method != http.MethodPost) || (selected && r.Method != http.MethodPut && r.Method != http.MethodDelete) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if kind == "recipients" {
		creds, e := readCredentials(s.Config().CredentialFile)
		if e != nil {
			fail(w, 500, e)
			return
		}
		if r.Method == http.MethodGet {
			respond(w, 200, map[string]any{"recipients": creds.CryptoRecipients})
			return
		}
		if r.Method == http.MethodDelete {
			delete(creds.CryptoRecipients, resource)
		} else {
			var input struct {
				Recipient string `json:"recipient"`
				PublicKey string `json:"public_key"`
			}
			if e = readJSON(w, r, &input); e != nil {
				fail(w, 400, e)
				return
			}
			if creds.CryptoRecipients == nil {
				creds.CryptoRecipients = map[string]string{}
			}
			if selected {
				delete(creds.CryptoRecipients, resource)
			}
			creds.CryptoRecipients[input.Recipient] = input.PublicKey
		}
		if e = WritePrivateJSON(s.Config().CredentialFile, creds); e != nil {
			fail(w, 500, e)
			return
		}
		s.emit(activity.Event{Type: "crypto_recipient", Audience: "security", Message: "Application Server crypto enrollment changed."})
		respond(w, 200, map[string]bool{"saved": true})
		return
	}
	keys, e := s.readTrafficKeys()
	if e != nil {
		fail(w, 500, errors.New("could not open protected key store"))
		return
	}
	if r.Method == http.MethodGet {
		for i := range keys {
			keys[i].Material = ""
		}
		respond(w, 200, map[string]any{"keys": keys, "algorithms": p25crypto.Algorithms()})
		return
	}
	index := -1
	var input trafficKey
	if selected {
		algorithmText, keyText, pair := strings.Cut(resource, "/")
		algorithm, algorithmError := strconv.Atoi(algorithmText)
		key, keyError := strconv.Atoi(keyText)
		if !pair || algorithmError != nil || keyError != nil {
			fail(w, 400, errors.New("could not read the selected encryption algorithm and Key ID"))
			return
		}
		index = slices.IndexFunc(keys, func(k trafficKey) bool { return k.AlgorithmID == algorithm && k.KeyID == key })
		input.AlgorithmID, input.KeyID = algorithm, key
	}
	if r.Method == http.MethodDelete {
		if index >= 0 {
			input = keys[index]
			keys = slices.Delete(keys, index, index+1)
		}
	} else {
		var fields struct {
			AlgorithmID int     `json:"algorithm_id"`
			KeyID       int     `json:"key_id"`
			Name        string  `json:"name"`
			Enabled     bool    `json:"enabled"`
			Material    *string `json:"material,omitempty"`
		}
		if e = readJSON(w, r, &fields); e != nil {
			fail(w, 400, errors.New("invalid key record"))
			return
		}
		input = trafficKey{AlgorithmID: fields.AlgorithmID, KeyID: fields.KeyID, Name: fields.Name, Enabled: fields.Enabled}
		if fields.Material != nil {
			input.Material = *fields.Material
		} else if index >= 0 {
			input.Material = keys[index].Material
		}
		if index >= 0 {
			keys[index] = input
		} else {
			keys = append(keys, input)
		}
	}
	if e = s.writeTrafficKeys(keys); e != nil {
		fail(w, 500, errors.New("could not save protected keys"))
		return
	}
	s.emit(activity.Event{Type: "crypto_key", Audience: "security", Message: fmt.Sprintf("Traffic key %d/%d changed.", input.AlgorithmID, input.KeyID)})
	respond(w, 200, map[string]bool{"saved": true})
}

func (s *Service) reconcileCrypto() (err error) {
	if s.Config().Role != RoleApplication {
		return nil
	}
	if s.crypto == nil {
		s.crypto = &keyRuntime{keys: map[p25crypto.Identity]bool{}}
	}
	runtime := s.crypto
	runtime.reconcileMu.Lock()
	defer runtime.reconcileMu.Unlock()
	defer func() {
		if err != nil {
			if old := runtime.detach(); old != nil {
				_ = old.Close()
			}
		}
	}()
	envelopes, e := s.keyEnvelopes()
	if e != nil {
		return e
	}
	binary := s.Config().Media.CodecBinary
	if binary == "" {
		executable, _ := os.Executable()
		candidate := filepath.Join(filepath.Dir(executable), "native", "dvmbridge")
		if info, e := os.Stat(candidate); e == nil && !info.IsDir() {
			binary = candidate
		}
	}
	if binary == "" {
		if old := runtime.detach(); old != nil {
			_ = old.Close()
		}
		return nil
	}
	keys := []trafficKey{}
	env, has := envelopes[s.Config().InstanceID]
	if has {
		private, e := endpointKey(s.Config().StateDir)
		if e != nil {
			return e
		}
		creds, e := readCredentials(s.Config().CredentialFile)
		if e != nil {
			return e
		}
		pin, e := hex.DecodeString(creds.CryptoSystemPublicKey)
		if e != nil || len(pin) != ed25519.PublicKeySize {
			return errors.New("Application Server requires its locally pinned System verification key")
		}
		keys, e = openKeyEnvelope(private, s.view().System.ID, s.Config().InstanceID, env, ed25519.PublicKey(pin))
		if e != nil {
			return e
		}
	}
	var native strings.Builder
	available := map[p25crypto.Identity]bool{}
	for _, key := range keys {
		if key.Enabled {
			material, e := hex.DecodeString(key.Material)
			if e != nil || p25crypto.ValidateKey(key.AlgorithmID, material) != nil {
				clear(material)
				return errors.New("invalid authorized traffic key")
			}
			clear(material)
			fmt.Fprintf(&native, "%d:%d:%s\n", key.AlgorithmID, key.KeyID, key.Material)
			available[p25crypto.Pair(key.AlgorithmID, key.KeyID)] = true
		}
	}
	text := native.String()
	sum := sha256.Sum256([]byte(binary + "\n" + text))
	digest := hex.EncodeToString(sum[:])
	old, _, previousDigest := runtime.snapshot()
	if previousDigest == digest && old != nil && old.Alive() {
		return nil
	}
	// Remove ownership first. Existing calls fail closed while the replacement
	// starts; readers never wait for file/process work under the snapshot lock.
	if old = runtime.detach(); old != nil {
		_ = old.Close()
	}
	directory := filepath.Join(s.Config().StateDir, "crypto")
	if e := os.MkdirAll(directory, 0700); e != nil {
		return e
	}
	path := filepath.Join(directory, "codec.keys")
	if e := writeRuntimeKeys(path, []byte(text)); e != nil {
		return e
	}
	defer os.Remove(path)
	codec, e := dispatch.NewPacketCodec(dispatch.PacketCodecOptions{Binary: binary, KeyFile: path})
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(s.operationCtx, 5*time.Second)
	defer cancel()
	if e = codec.Start(ctx); e != nil {
		_ = codec.Close()
		return e
	}
	runtime.mu.Lock()
	runtime.codec = codec
	runtime.digest = digest
	runtime.keys = available
	runtime.mu.Unlock()
	return nil
}
func (s *Service) packetCodec() (*dispatch.PacketCodec, map[p25crypto.Identity]bool) {
	if s.crypto == nil {
		return nil, nil
	}
	codec, keys, _ := s.crypto.snapshot()
	return codec, keys
}

func (s *Service) loadedTrafficKeys() []dispatch.KeyIdentity {
	_, loaded := s.packetCodec()
	keys := make([]dispatch.KeyIdentity, 0, len(loaded))
	for key := range loaded {
		keys = append(keys, dispatch.KeyIdentity{AlgorithmID: key.AlgorithmID, KeyID: key.KeyID})
	}
	slices.SortFunc(keys, func(a, b dispatch.KeyIdentity) int {
		return cmp.Or(cmp.Compare(a.AlgorithmID, b.AlgorithmID), cmp.Compare(a.KeyID, b.KeyID))
	})
	return keys
}
