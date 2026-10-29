package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"
)

// The servers otiumctl is signed in to, kept as gh keeps them:
// hosts.yaml in the config directory names each server and who is signed in there; the token
// itself is in the OS keychain, or in hosts.yaml (mode 0600) where there is no keychain.
// OTIUM_TOKEN and OTIUM_API_URL override both, for CI.

// defaultHost is the production API, signed in to when nothing names another. Like gh and
// github.com: the common case needs no flag, and staging or a dev server is one --hostname away.
const defaultHost = "api.getotium.ai"

// errNotSignedIn means no credential was found for the server.
var errNotSignedIn = errors.New("not signed in")

// hostEntry is one server.
type hostEntry struct {
	// URL is the Otium API, e.g. https://api.getotium.ai: the one host every command talks to.
	URL string `yaml:"url"`
	// User describes the credential, as the server reported it at sign-in.
	User string `yaml:"user,omitempty"`
	// Token, Refresh and ExpiresAt are set only when there was no keychain to keep them in.
	Token     string    `yaml:"token,omitempty"`
	Refresh   string    `yaml:"refresh_token,omitempty"`
	ExpiresAt time.Time `yaml:"expires_at,omitempty"`
}

// secrets is what a sign-in keeps: the access token, and for a person's sign-in the refresh
// token that renews it and when the access token lapses. An API key is an access token alone.
type secrets struct {
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// encode is the keychain form: a bare API key stays bare; a sign-in with a refresh token is a
// small JSON document.
func (s secrets) encode() string {
	if s.Refresh == "" {
		return s.Access
	}
	b, _ := json.Marshal(s) //nolint:gosec // serialized FOR the keychain, which is where secrets belong
	return string(b)
}

func decodeSecrets(v string) secrets {
	var s secrets
	if strings.HasPrefix(v, "{") && json.Unmarshal([]byte(v), &s) == nil && s.Access != "" {
		return s
	}
	return secrets{Access: v}
}

// hostsFile is hosts.yaml.
type hostsFile struct {
	// Default is the server commands use without --hostname.
	Default string                `yaml:"default,omitempty"`
	Hosts   map[string]*hostEntry `yaml:"hosts,omitempty"`
}

// keyringAPI is the part of the OS keychain the store uses; tests replace it.
type keyringAPI interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
	Delete(service, user string) error
}

// osKeyring is the system keychain (Windows Credential Manager, macOS Keychain, the Secret
// Service on Linux), through the library gh uses.
type osKeyring struct{}

func (osKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (osKeyring) Set(service, user, secret string) error   { return keyring.Set(service, user, secret) }
func (osKeyring) Delete(service, user string) error        { return keyring.Delete(service, user) }

// hostStore reads and writes hosts.yaml and the keychain.
type hostStore struct {
	dir     string
	keyring keyringAPI
}

// openHosts is the credential store commands use; tests replace it.
var openHosts = defaultHosts

// defaultHosts is the store in OTIUM_CONFIG_DIR, or the platform's config directory
// (%AppData%\otium on Windows, ~/.config/otium on Linux).
func defaultHosts() (*hostStore, error) {
	dir := os.Getenv("OTIUM_CONFIG_DIR")
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(base, "otium")
	}
	return &hostStore{dir: dir, keyring: osKeyring{}}, nil
}

func (s *hostStore) path() string { return filepath.Join(s.dir, "hosts.yaml") }

// load reads hosts.yaml; a missing file is an empty one.
func (s *hostStore) load() (*hostsFile, error) {
	f := &hostsFile{Hosts: map[string]*hostEntry{}}
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path(), err)
	}
	if f.Hosts == nil {
		f.Hosts = map[string]*hostEntry{}
	}
	return f, nil
}

// save writes hosts.yaml, readable only by the user.
func (s *hostStore) save(f *hostsFile) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(), b, 0o600)
}

// hostName turns what the user typed (a URL or a bare host name) into the server's name and
// base URL. A bare name means HTTPS.
func hostName(s string) (name, base string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", errors.New("no server given")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") || (u.Scheme != "https" && u.Scheme != "http") {
		return "", "", fmt.Errorf("%q is not a server URL", s)
	}
	return u.Host, u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"), nil
}

func keyringService(name string) string { return "otium:" + name }

// credWhere says where a credential is kept.
type credWhere string

const (
	inKeyring credWhere = "keychain"
	inFile    credWhere = "file"
	inEnv     credWhere = "environment"
	inFlag    credWhere = "flag"
)

// signIn keeps a server's credentials, replacing any earlier ones: in the keychain, or in
// hosts.yaml when there is none (or insecure is set). The server becomes the default when there
// is no other.
func (s *hostStore) signIn(name, base, user string, sec secrets, insecure bool) (credWhere, error) {
	f, err := s.load()
	if err != nil {
		return "", err
	}
	h := &hostEntry{URL: base, User: user}
	where := inKeyring
	if insecure || s.keyring.Set(keyringService(name), name, sec.encode()) != nil {
		h.Token, h.Refresh, h.ExpiresAt, where = sec.Access, sec.Refresh, sec.ExpiresAt, inFile
	}
	f.Hosts[name] = h
	if f.Default == "" || f.Hosts[f.Default] == nil {
		f.Default = name
	}
	return where, s.save(f)
}

// renewed replaces a sign-in's credentials after a refresh, where they were kept.
func (s *hostStore) renewed(name string, sec secrets) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	h := f.Hosts[name]
	if h == nil {
		return fmt.Errorf("%w to %s", errNotSignedIn, name)
	}
	if h.Token == "" {
		return s.keyring.Set(keyringService(name), name, sec.encode())
	}
	h.Token, h.Refresh, h.ExpiresAt = sec.Access, sec.Refresh, sec.ExpiresAt
	return s.save(f)
}

// signOut forgets a server and its token. The next server alphabetically becomes the default.
func (s *hostStore) signOut(name string) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := f.Hosts[name]; !ok {
		return fmt.Errorf("%w to %s", errNotSignedIn, name)
	}
	_ = s.keyring.Delete(keyringService(name), name)
	delete(f.Hosts, name)
	if f.Default == name {
		f.Default = ""
		names := make([]string, 0, len(f.Hosts))
		for n := range f.Hosts {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) > 0 {
			f.Default = names[0]
		}
	}
	return s.save(f)
}

// credential is what a command calls the API with.
type credential struct {
	Name, URL, Token, User string
	Where                  credWhere
	// Refresh and ExpiresAt are a person's sign-in: the token that renews Token, and when Token
	// lapses. Empty for an API key.
	Refresh   string
	ExpiresAt time.Time
}

// envToken is the token CI supplies: OTIUM_TOKEN, or OTIUM_API_KEY, the name otiumctl read
// before it could sign in.
func envToken() string { return firstNonEmpty(os.Getenv("OTIUM_TOKEN"), os.Getenv("OTIUM_API_KEY")) }

// resolve finds the server and token for a command: the server given (or OTIUM_API_URL, or the
// default), and a token given (or OTIUM_TOKEN, or the one stored for that server).
func (s *hostStore) resolve(server, token string) (credential, error) {
	if server == "" {
		server = os.Getenv("OTIUM_API_URL")
	}
	f, err := s.load()
	if err != nil {
		return credential{}, err
	}
	if server == "" {
		server = f.Default
	}
	if server == "" {
		return credential{}, fmt.Errorf("%w: run otiumctl auth login --hostname <api host>", errNotSignedIn)
	}
	name, base, err := hostName(server)
	if err != nil {
		return credential{}, err
	}
	switch {
	case token != "":
		return credential{Name: name, URL: s.urlFor(f, name, base), Token: token, Where: inFlag}, nil
	case envToken() != "":
		return credential{Name: name, URL: s.urlFor(f, name, base), Token: envToken(), Where: inEnv}, nil
	}
	return s.storedIn(f, name)
}

// urlFor is the base URL recorded at sign-in for a server, or base.
func (s *hostStore) urlFor(f *hostsFile, name, base string) string {
	if h := f.Hosts[name]; h != nil && h.URL != "" {
		return h.URL
	}
	return base
}

// stored is the sign-in kept for a server, whatever the environment says.
func (s *hostStore) stored(name string) (credential, error) {
	f, err := s.load()
	if err != nil {
		return credential{}, err
	}
	return s.storedIn(f, name)
}

func (s *hostStore) storedIn(f *hostsFile, name string) (credential, error) {
	h := f.Hosts[name]
	if h == nil {
		return credential{}, fmt.Errorf("%w to %s: run otiumctl auth login --hostname %s", errNotSignedIn, name, name)
	}
	c := credential{Name: name, URL: h.URL, User: h.User}
	if h.Token != "" {
		c.Token, c.Refresh, c.ExpiresAt, c.Where = h.Token, h.Refresh, h.ExpiresAt, inFile
		return c, nil
	}
	v, err := s.keyring.Get(keyringService(name), name)
	if err != nil {
		return credential{}, fmt.Errorf("%w to %s: run otiumctl auth login --hostname %s (the keychain has no token: %w)",
			errNotSignedIn, name, name, err)
	}
	sec := decodeSecrets(v)
	c.Token, c.Refresh, c.ExpiresAt, c.Where = sec.Access, sec.Refresh, sec.ExpiresAt, inKeyring
	return c, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
