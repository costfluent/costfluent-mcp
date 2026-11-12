package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/costfluent/costfluent-mcp/internal/config"
)

// ErrNotSignedIn is a resource no store holds a sign-in for.
var ErrNotSignedIn = errors.New("not signed in")

// Store keeps sign-ins, one per API base URL, so a sign-in to a development API never replaces the
// production one.
type Store interface {
	// Load returns resource's sign-in, or ErrNotSignedIn.
	Load(ctx context.Context, resource string) (*Credential, error)
	Save(ctx context.Context, cred *Credential) error
	// Delete forgets resource's sign-in; forgetting none is no error.
	Delete(ctx context.Context, resource string) error
	// Where names the store in messages.
	Where() string
}

// Keyring is the operating system's credential store as go-keyring reaches it: the macOS Keychain,
// the Secret Service on Linux, the Windows Credential Manager.
type Keyring interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

// SystemKeyring is the operating system's credential store.
type SystemKeyring struct{}

func (SystemKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (SystemKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (SystemKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

// keyringService is the service every sign-in is filed under; the API base URL is its account.
const keyringService = "costfluent"

// keyringTimeout is how long one call to the credential store is waited for: long enough to answer
// an unlock prompt, short enough that a prompt no one sees, as on a desktop reached over SSH, does
// not hang the command.
const keyringTimeout = 30 * time.Second

// errKeyringTimeout is a credential store that did not answer in time.
var errKeyringTimeout = errors.New("the system credential store did not answer")

// KeyringStore keeps each sign-in as one item in the operating system's credential store.
type KeyringStore struct {
	Keyring Keyring
	// Timeout bounds each call; zero is keyringTimeout.
	Timeout time.Duration
}

func (s KeyringStore) Where() string { return "the system credential store" }

// call runs op until it answers, ctx ends or the timeout passes. go-keyring takes no context and can
// wait on an unlock prompt for ever, so op runs on its own goroutine, which is left to finish, or
// to wait, after this returns.
func (s KeyringStore) call(ctx context.Context, op func() (string, error)) (string, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = keyringTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type answer struct {
		value string
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		v, err := op()
		done <- answer{v, err}
	}()
	select {
	case a := <-done:
		return a.value, a.err
	case <-bounded.Done():
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("waiting for the system credential store: %w", err)
		}
		return "", fmt.Errorf("%w within %s, as when it waits for an unlock no one answers: %w; unlock it, keep the sign-in in a file "+
			"with costfluent login --insecure-storage, or set %s", errKeyringTimeout, timeout, bounded.Err(), config.EnvAPIToken)
	}
}

func (s KeyringStore) Load(ctx context.Context, resource string) (*Credential, error) {
	raw, err := s.call(ctx, func() (string, error) { return s.Keyring.Get(keyringService, resource) })
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNotSignedIn
	}
	if err != nil {
		return nil, fmt.Errorf("reading the system credential store: %w", err)
	}
	var cred Credential
	if err := json.Unmarshal([]byte(raw), &cred); err != nil || cred.Resource != resource || cred.RefreshToken == "" {
		return nil, fmt.Errorf("the system credential store's %s item for %s is not a sign-in this command wrote; sign in again", keyringService, resource)
	}
	return &cred, nil
}

func (s KeyringStore) Save(ctx context.Context, cred *Credential) error {
	raw, err := json.Marshal(cred) //nolint:gosec // keeping the tokens is this store's job; the credential store guards them
	if err != nil {
		return err
	}
	if _, err := s.call(ctx, func() (string, error) { return "", s.Keyring.Set(keyringService, cred.Resource, string(raw)) }); err != nil {
		return fmt.Errorf("writing the system credential store: %w", err)
	}
	return nil
}

func (s KeyringStore) Delete(ctx context.Context, resource string) error {
	_, err := s.call(ctx, func() (string, error) { return "", s.Keyring.Delete(keyringService, resource) })
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("deleting from the system credential store: %w", err)
	}
	return nil
}

// FileStore keeps sign-ins in one JSON file, by API base URL, readable and writable by its owner
// alone: only for a machine without a credential store, and only when the user asks for it.
type FileStore struct {
	Path string
}

func (s FileStore) Where() string { return s.Path }

func (s FileStore) Load(_ context.Context, resource string) (*Credential, error) {
	all, err := s.read()
	if err != nil {
		return nil, err
	}
	cred, ok := all[resource]
	if !ok || cred == nil {
		return nil, ErrNotSignedIn
	}
	return cred, nil
}

func (s FileStore) Save(_ context.Context, cred *Credential) error {
	all, err := s.read()
	if err != nil {
		return err
	}
	all[cred.Resource] = cred
	return s.write(all)
}

func (s FileStore) Delete(_ context.Context, resource string) error {
	all, err := s.read()
	if err != nil {
		return err
	}
	if _, ok := all[resource]; !ok {
		return nil
	}
	delete(all, resource)
	return s.write(all)
}

// read returns the file's sign-ins, none when it does not exist. A file others may read or write is
// refused rather than used or tightened: someone may already have read its tokens, so it is the
// user's to judge, and chmod or delete. Windows keeps no such mode bits; the file there sits in the
// profile, which its access control list closes to other users.
func (s FileStore) read() (map[string]*Credential, error) {
	if s.Path == "" {
		return nil, errors.New("no configuration directory is known for the credentials file; set XDG_CONFIG_HOME")
	}
	info, err := os.Stat(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]*Credential{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", s.Path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s holds sign-in tokens but its mode is %04o, so others may read or change it; "+
			"make it readable by you alone with chmod 600 %s, or delete it and sign in again", s.Path, info.Mode().Perm(), s.Path)
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	all := map[string]*Credential{}
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("%s is not a credentials file this command wrote: %w", s.Path, err)
	}
	return all, nil
}

// write replaces the file through a temporary file in its directory, which os.CreateTemp creates
// 0600 before any token is written to it, so the file is never seen half written or with a wider mode. The last sign-in
// forgotten removes the file.
func (s FileStore) write(all map[string]*Credential) error {
	if len(all) == 0 {
		if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := json.MarshalIndent(all, "", "  ") //nolint:gosec // keeping the tokens is this store's job, in a file only its owner reads
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// FilePath is the credentials file: credentials.json in costfluent under XDG_CONFIG_HOME when set,
// else under the operating system's configuration directory.
func FilePath(getenv func(string) string, userConfigDir func() (string, error)) (string, error) {
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		var err error
		if dir, err = userConfigDir(); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, "costfluent", "credentials.json"), nil
}

// Stores is where sign-ins are kept: the operating system's credential store, and the file the
// user chose instead with an explicit flag.
type Stores struct {
	Keyring Store
	File    Store
}

// Load returns resource's sign-in and the store holding it, the credential store first. A
// credential store that cannot be read, as on a machine without one, leaves the file to answer;
// when neither holds the sign-in, the error says why the credential store could not.
func (s *Stores) Load(ctx context.Context, resource string) (*Credential, Store, error) {
	cred, keyringErr := s.Keyring.Load(ctx, resource)
	if keyringErr == nil {
		return cred, s.Keyring, nil
	}
	cred, err := s.File.Load(ctx, resource)
	if err == nil {
		return cred, s.File, nil
	}
	if !errors.Is(err, ErrNotSignedIn) {
		return nil, nil, err
	}
	if !errors.Is(keyringErr, ErrNotSignedIn) {
		return nil, nil, fmt.Errorf("%w (%w)", ErrNotSignedIn, keyringErr)
	}
	return nil, nil, ErrNotSignedIn
}

// Save keeps cred in the credential store, or in the file when inFile, and forgets any sign-in for
// the same API the other one holds, so a later Load never finds a spent one. It returns the store
// that kept it, and nil with the error when none did. A store returned with an error kept cred, but
// the other store could not forget its copy: that copy may be found again, and live, once its store
// can be reached, as a credential store that was locked or unreachable at sign-in can.
func (s *Stores) Save(ctx context.Context, cred *Credential, inFile bool) (Store, error) {
	keep, other := s.Keyring, s.File
	if inFile {
		keep, other = s.File, s.Keyring
	}
	if err := keep.Save(ctx, cred); err != nil {
		return nil, err
	}
	if err := other.Delete(ctx, cred.Resource); err != nil {
		return keep, fmt.Errorf("could not forget a sign-in to %s that %s may hold: %w", cred.Resource, other.Where(), err)
	}
	return keep, nil
}
