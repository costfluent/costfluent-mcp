package login

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// fakeKeyring is a credential store in memory; err, when set, is what every call answers, as a
// machine without a Secret Service answers.
type fakeKeyring struct {
	items map[string]string
	err   error
}

func newFakeKeyring() *fakeKeyring { return &fakeKeyring{items: map[string]string{}} }

func (k *fakeKeyring) Get(service, user string) (string, error) {
	if k.err != nil {
		return "", k.err
	}
	v, ok := k.items[service+"/"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (k *fakeKeyring) Set(service, user, password string) error {
	if k.err != nil {
		return k.err
	}
	k.items[service+"/"+user] = password
	return nil
}

func (k *fakeKeyring) Delete(service, user string) error {
	if k.err != nil {
		return k.err
	}
	if _, ok := k.items[service+"/"+user]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.items, service+"/"+user)
	return nil
}

var errNoSecretService = errors.New("the name org.freedesktop.secrets was not provided by any .service files")

func credentialFor(resource string) *Credential {
	return &Credential{Resource: resource, Issuer: "https://app.costfluent.com", TokenEndpoint: "https://app.costfluent.com/api/oauth/token",
		RevocationEndpoint: "https://app.costfluent.com/api/oauth/revoke", AccessToken: "cf_oat_a", ExpiresAt: clock.Add(time.Hour), RefreshToken: "cf_ort_a"}
}

func TestKeyringStoreKeepsOneSignInPerAPI(t *testing.T) {
	k := newFakeKeyring()
	s := KeyringStore{Keyring: k}
	if _, err := s.Load(context.Background(), "https://api.costfluent.com"); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("empty: err = %v", err)
	}
	prod, dev := credentialFor("https://api.costfluent.com"), credentialFor("https://api.costfluent.dev")
	dev.RefreshToken = "cf_ort_dev"
	for _, c := range []*Credential{prod, dev} {
		if err := s.Save(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Load(context.Background(), prod.Resource)
	if err != nil || *got != *prod {
		t.Fatalf("loaded %v, %v", got, err)
	}
	if _, ok := k.items["costfluent/https://api.costfluent.com"]; !ok {
		t.Errorf("items %v, want one under the costfluent service named by the API", k.items)
	}
	if err := s.Delete(context.Background(), prod.Resource); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), prod.Resource); err != nil {
		t.Errorf("deleting nothing: %v", err)
	}
	if got, err := s.Load(context.Background(), dev.Resource); err != nil || got.RefreshToken != "cf_ort_dev" {
		t.Errorf("deleting one API's sign-in touched the other's: %v, %v", got, err)
	}
	if s.Where() != "the system credential store" {
		t.Errorf("where %q", s.Where())
	}
}

func TestKeyringStoreRefusesAnItemItDidNotWrite(t *testing.T) {
	k := newFakeKeyring()
	s := KeyringStore{Keyring: k}
	for name, raw := range map[string]string{
		"not JSON":         "hunter2",
		"another resource": `{"resource":"https://evil.example","refresh_token":"x"}`,
		"no refresh token": `{"resource":"https://api.costfluent.com"}`,
	} {
		k.items["costfluent/https://api.costfluent.com"] = raw
		if _, err := s.Load(context.Background(), "https://api.costfluent.com"); err == nil || errors.Is(err, ErrNotSignedIn) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestKeyringStoreReportsAnUnavailableStore(t *testing.T) {
	s := KeyringStore{Keyring: &fakeKeyring{err: errNoSecretService}}
	if _, err := s.Load(context.Background(), "https://api.costfluent.com"); !errors.Is(err, errNoSecretService) || errors.Is(err, ErrNotSignedIn) {
		t.Errorf("load: %v", err)
	}
	if err := s.Save(context.Background(), credentialFor("https://api.costfluent.com")); !errors.Is(err, errNoSecretService) {
		t.Errorf("save: %v", err)
	}
	if err := s.Delete(context.Background(), "https://api.costfluent.com"); !errors.Is(err, errNoSecretService) {
		t.Errorf("delete: %v", err)
	}
}

func TestFileStoreWritesAFileOnlyItsOwnerReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "costfluent", "credentials.json")
	s := FileStore{Path: path}
	if _, err := s.Load(context.Background(), "https://api.costfluent.com"); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("no file: err = %v", err)
	}
	prod, dev := credentialFor("https://api.costfluent.com"), credentialFor("https://api.costfluent.dev")
	if err := s.Save(context.Background(), prod); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), dev); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("file mode %v, %v, want 0600", info.Mode().Perm(), err)
		}
		dir, err := os.Stat(filepath.Dir(path))
		if err != nil || dir.Mode().Perm() != 0o700 {
			t.Errorf("directory mode %v, %v, want 0700", dir.Mode().Perm(), err)
		}
	}
	got, err := s.Load(context.Background(), dev.Resource)
	if err != nil || *got != *dev {
		t.Fatalf("loaded %v, %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("the directory holds %d files, want the credentials file alone (no temporary file left)", len(entries))
	}
	if err := s.Delete(context.Background(), prod.Resource); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(context.Background(), "https://never.example"); err != nil {
		t.Errorf("deleting nothing: %v", err)
	}
	if _, err := s.Load(context.Background(), prod.Resource); !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("deleted, still loads: %v", err)
	}
	if err := s.Delete(context.Background(), dev.Resource); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the last sign-in forgotten leaves the file: %v", err)
	}
	if s.Where() != path {
		t.Errorf("where %q", s.Where())
	}
}

func TestFileStoreRefusesAFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no Unix mode bits")
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	s := FileStore{Path: path}
	if err := s.Save(context.Background(), credentialFor("https://api.costfluent.com")); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o606} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := s.Load(context.Background(), "https://api.costfluent.com")
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%04o", mode)) || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("mode %04o: load err = %v", mode, err)
		}
		if err := s.Save(context.Background(), credentialFor("https://api.costfluent.com")); err == nil {
			t.Errorf("mode %04o: save overwrote it", mode)
		}
		if err := s.Delete(context.Background(), "https://api.costfluent.com"); err == nil {
			t.Errorf("mode %04o: delete went ahead", mode)
		}
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(context.Background(), "https://api.costfluent.com"); err != nil {
		t.Errorf("a read-only file of its owner: %v", err)
	}
}

func TestFileStoreRefusesWhatItDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := (FileStore{Path: dir}).Load(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory: %v", err)
	}
	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileStore{Path: path}).Load(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "not a credentials file") {
		t.Errorf("not JSON: %v", err)
	}
	if _, err := (FileStore{}).Load(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "XDG_CONFIG_HOME") {
		t.Errorf("no path: %v", err)
	}
	blocked := filepath.Join(path, "costfluent", "credentials.json") // under a file, so no directory can be made
	if err := (FileStore{Path: blocked}).Save(context.Background(), credentialFor("x")); err == nil {
		t.Errorf("an uncreatable directory: no error")
	}
}

func TestFilePath(t *testing.T) {
	sys := func() (string, error) { return "/home/u/.config", nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got, _ := FilePath(env(map[string]string{"XDG_CONFIG_HOME": "/xdg"}), sys); got != filepath.Join("/xdg", "costfluent", "credentials.json") {
		t.Errorf("XDG_CONFIG_HOME: %s", got)
	}
	// The XDG spec ignores a relative path, which would put tokens wherever the command runs.
	if got, _ := FilePath(env(map[string]string{"XDG_CONFIG_HOME": "rel"}), sys); got != filepath.Join("/home/u/.config", "costfluent", "credentials.json") {
		t.Errorf("relative XDG_CONFIG_HOME: %s", got)
	}
	if got, _ := FilePath(env(nil), sys); got != filepath.Join("/home/u/.config", "costfluent", "credentials.json") {
		t.Errorf("fallback: %s", got)
	}
	if _, err := FilePath(env(nil), func() (string, error) { return "", errors.New("$HOME is not defined") }); err == nil {
		t.Errorf("no configuration directory: no error")
	}
}

func TestStoresPreferTheCredentialStoreAndKeepOneCopy(t *testing.T) {
	const api = "https://api.costfluent.com"
	k := newFakeKeyring()
	stores := &Stores{Keyring: KeyringStore{Keyring: k}, File: FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}}
	if _, _, err := stores.Load(context.Background(), api); !errors.Is(err, ErrNotSignedIn) || err.Error() != ErrNotSignedIn.Error() {
		t.Fatalf("nothing stored: %v", err)
	}

	inFile := credentialFor(api)
	inFile.RefreshToken = "cf_ort_file"
	store, err := stores.Save(context.Background(), inFile, true)
	if err != nil || store != stores.File {
		t.Fatalf("saved in %v, %v", store, err)
	}
	got, store, err := stores.Load(context.Background(), api)
	if err != nil || store != stores.File || got.RefreshToken != "cf_ort_file" {
		t.Fatalf("loaded %v from %v, %v", got, store, err)
	}

	// Signing in again into the credential store forgets the file's copy.
	if store, err := stores.Save(context.Background(), credentialFor(api), false); err != nil || store != stores.Keyring {
		t.Fatalf("saved in %v, %v", store, err)
	}
	if _, err := stores.File.Load(context.Background(), api); !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("the file still holds the sign-in: %v", err)
	}
	got, store, err = stores.Load(context.Background(), api)
	if err != nil || store != stores.Keyring || got.RefreshToken != "cf_ort_a" {
		t.Fatalf("loaded %v from %v, %v", got, store, err)
	}

	// And into the file forgets the credential store's.
	if _, err := stores.Save(context.Background(), inFile, true); err != nil {
		t.Fatal(err)
	}
	if len(k.items) != 0 {
		t.Errorf("the credential store still holds %v", k.items)
	}
}

func TestStoresFallBackToTheFileWithoutACredentialStore(t *testing.T) {
	const api = "https://api.costfluent.com"
	stores := &Stores{Keyring: KeyringStore{Keyring: &fakeKeyring{err: errNoSecretService}}, File: FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}}
	_, _, err := stores.Load(context.Background(), api)
	if !errors.Is(err, ErrNotSignedIn) || !errors.Is(err, errNoSecretService) {
		t.Fatalf("err = %v, want not signed in, saying why the credential store could not answer", err)
	}
	if _, err := stores.Save(context.Background(), credentialFor(api), false); !errors.Is(err, errNoSecretService) {
		t.Fatalf("saving without a credential store: %v", err)
	}
	// Kept in the file, with the copy a credential store that could not be reached may hold reported.
	if store, err := stores.Save(context.Background(), credentialFor(api), true); store != stores.File || !errors.Is(err, errNoSecretService) {
		t.Fatalf("saving in the file: %v, %v", store, err)
	}
	if _, store, err := stores.Load(context.Background(), api); err != nil || store != stores.File {
		t.Fatalf("loaded from %v, %v", store, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(stores.File.Where(), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := stores.Load(context.Background(), api); err == nil || errors.Is(err, ErrNotSignedIn) {
			t.Errorf("a file others can read: err = %v, want it refused", err)
		}
	}
}

// A copy the other store could not forget is reported beside the store that kept the sign-in, so a
// caller can say a sign-in may outlive the next logout's view of one store.
func TestStoresSaveReportsACopyItCouldNotForget(t *testing.T) {
	const api = "https://api.costfluent.com"
	k := newFakeKeyring()
	stores := &Stores{Keyring: KeyringStore{Keyring: k}, File: FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}}
	if _, err := stores.Save(context.Background(), credentialFor(api), false); err != nil {
		t.Fatal(err)
	}
	k.err = errNoSecretService
	inFile := credentialFor(api)
	inFile.RefreshToken = "cf_ort_file"
	store, err := stores.Save(context.Background(), inFile, true)
	if store != stores.File || !errors.Is(err, errNoSecretService) || !strings.Contains(err.Error(), "the system credential store") {
		t.Fatalf("saved in %v, err %v; want the file, and the credential store's copy reported", store, err)
	}
	if got, err := stores.File.Load(context.Background(), api); err != nil || got.RefreshToken != "cf_ort_file" {
		t.Errorf("the file holds %v, %v", got, err)
	}
}

func TestCredentialNeverPrintsItsTokens(t *testing.T) {
	c := credentialFor("https://api.costfluent.com")
	for _, s := range []string{fmt.Sprint(*c), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", *c), fmt.Sprintf("%#v", *c)} {
		if strings.Contains(s, "cf_oat_") || strings.Contains(s, "cf_ort_") {
			t.Errorf("printed %q", s)
		}
	}
}

// session is a session over s's server whose clock the test moves.
func (s *authServer) session(t *testing.T, now *time.Time) (*Session, *fakeKeyring) {
	t.Helper()
	k := newFakeKeyring()
	return &Session{
		Stores: &Stores{Keyring: KeyringStore{Keyring: k}, File: FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}},
		HTTP:   NewHTTPClient(),
		Now:    func() time.Time { return *now },
	}, k
}

func TestSessionRefreshesAnExpiredTokenAndKeepsTheRotatedOne(t *testing.T) {
	s := newAuthServer(t)
	now := clock
	session, _ := s.session(t, &now)
	cred, err := s.flow(io.Discard).Login(context.Background(), s.api.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Stores.Save(context.Background(), cred, false); err != nil {
		t.Fatal(err)
	}

	token, err := session.AccessToken(context.Background(), s.api.URL)
	if err != nil || token != "cf_oat_1" || len(s.tokenForms) != 1 {
		t.Fatalf("a live token: %q, %v, %d token requests", token, err, len(s.tokenForms))
	}

	now = clock.Add(time.Hour - 30*time.Second) // within the skew of its expiry
	token, err = session.AccessToken(context.Background(), s.api.URL)
	if err != nil || token != "cf_oat_2" {
		t.Fatalf("an expiring token: %q, %v", token, err)
	}
	form := s.tokenForms[1]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "cf_ort_1" || form.Get("client_id") != ClientID || form.Get("resource") != s.api.URL {
		t.Errorf("refresh request %v", form)
	}
	kept, _, err := session.Stores.Load(context.Background(), s.api.URL)
	if err != nil || kept.RefreshToken != "cf_ort_2" || kept.AccessToken != "cf_oat_2" || !kept.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("kept %v (refresh %s), %v; want the rotated refresh token", kept, kept.RefreshToken, err)
	}

	// The spent refresh token is refused, as the server refuses a reuse.
	if _, err := Refresh(context.Background(), session.HTTP, cred, now); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the spent refresh token: %v", err)
	}
}

func TestSessionReportsARefusedRefresh(t *testing.T) {
	s := newAuthServer(t)
	now := clock
	session, k := s.session(t, &now)
	cred := credentialFor(s.api.URL)
	cred.TokenEndpoint = s.as.URL + "/api/oauth/token"
	cred.ExpiresAt = clock.Add(-time.Minute)
	if _, err := session.Stores.Save(context.Background(), cred, false); err != nil {
		t.Fatal(err)
	}
	_, err := session.AccessToken(context.Background(), s.api.URL)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if len(k.items) != 1 {
		t.Errorf("a refused refresh changed the store: %v", k.items)
	}
}

func TestSessionReportsARefreshItCouldNotKeep(t *testing.T) {
	s := newAuthServer(t)
	now := clock.Add(2 * time.Hour)
	session, _ := s.session(t, &now)
	cred, err := s.flow(io.Discard).Login(context.Background(), s.api.URL)
	if err != nil {
		t.Fatal(err)
	}
	session.Stores.Keyring = failingSave{Store: KeyringStore{Keyring: newFakeKeyringWith(cred)}}
	_, err = session.AccessToken(context.Background(), s.api.URL)
	if err == nil || !strings.Contains(err.Error(), "the keychain is locked") || !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshReportsAnUnreachableServer(t *testing.T) {
	cred := credentialFor("https://api.costfluent.com")
	cred.TokenEndpoint = "http://127.0.0.1:1/token"
	_, err := Refresh(context.Background(), NewHTTPClient(), cred, clock)
	if err == nil || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "refreshing the sign-in") {
		t.Errorf("err = %v, want a failure that is not a refusal", err)
	}
	if _, err := (&Session{Stores: &Stores{Keyring: KeyringStore{Keyring: newFakeKeyring()}, File: FileStore{Path: filepath.Join(t.TempDir(), "c.json")}}}).
		AccessToken(context.Background(), "https://api.costfluent.com"); !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("no sign-in: %v", err)
	}
}

func newFakeKeyringWith(c *Credential) *fakeKeyring {
	k := newFakeKeyring()
	_ = KeyringStore{Keyring: k}.Save(context.Background(), c)
	return k
}

// failingSave is a store that loads but cannot save, as a credential store locked mid-run.
type failingSave struct{ Store }

func (failingSave) Save(context.Context, *Credential) error {
	return errors.New("the keychain is locked")
}

func TestRevokeRevokesTheRefreshToken(t *testing.T) {
	s := newAuthServer(t)
	cred := credentialFor(s.api.URL)
	cred.RevocationEndpoint = s.as.URL + "/api/oauth/revoke"
	if err := Revoke(context.Background(), NewHTTPClient(), cred); err != nil {
		t.Fatal(err)
	}
	form := s.revoked[0]
	if form.Get("token") != "cf_ort_a" || form.Get("token_type_hint") != "refresh_token" || form.Get("client_id") != ClientID {
		t.Errorf("revocation %v", form)
	}

	s.revokeStatus = 400
	if err := Revoke(context.Background(), NewHTTPClient(), cred); err == nil || !strings.Contains(err.Error(), "unsupported_token_type") {
		t.Errorf("a refused revocation: %v", err)
	}
	cred.RevocationEndpoint = "http://127.0.0.1:1/revoke"
	if err := Revoke(context.Background(), NewHTTPClient(), cred); err == nil {
		t.Errorf("an unreachable server: no error")
	}
	cred.RevocationEndpoint = ""
	if err := Revoke(context.Background(), NewHTTPClient(), cred); !errors.Is(err, ErrNoRevocation) {
		t.Errorf("no endpoint: %v", err)
	}
	cred.RevocationEndpoint = "http://bad host/revoke"
	if err := Revoke(context.Background(), NewHTTPClient(), cred); err == nil {
		t.Errorf("an unparseable endpoint: no error")
	}
}

func TestRequestsAreBoundedAndCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cred := credentialFor("https://api.costfluent.com")
	cred.TokenEndpoint = "https://127.0.0.1:1/token"
	if _, err := Refresh(ctx, NewHTTPClient(), cred, clock); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled refresh: %v", err)
	}
	if err := getJSON(ctx, NewHTTPClient(), "http://bad host", nil); err == nil {
		t.Errorf("an unparseable address: no error")
	}
}

func TestSystemKeyringReachesGoKeyring(t *testing.T) {
	keyring.MockInit() // go-keyring's in-memory provider, for this test binary alone
	s := KeyringStore{Keyring: SystemKeyring{}}
	c := credentialFor("https://api.costfluent.com")
	if err := s.Save(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(context.Background(), c.Resource); err != nil || *got != *c {
		t.Fatalf("loaded %v, %v", got, err)
	}
	if err := s.Delete(context.Background(), c.Resource); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(context.Background(), c.Resource); !errors.Is(err, ErrNotSignedIn) {
		t.Errorf("deleted, still loads: %v", err)
	}
}

// hungKeyring is a credential store that never answers, as a locked Secret Service collection whose
// unlock prompt no one answers; its calls return once the test ends.
type hungKeyring struct{ release chan struct{} }

func newHungKeyring(t *testing.T) *hungKeyring {
	k := &hungKeyring{release: make(chan struct{})}
	t.Cleanup(func() { close(k.release) })
	return k
}

func (k *hungKeyring) Get(string, string) (string, error) {
	<-k.release
	return "", keyring.ErrNotFound
}

func (k *hungKeyring) Set(string, string, string) error { <-k.release; return nil }
func (k *hungKeyring) Delete(string, string) error      { <-k.release; return nil }

// returnsWithin fails the test when call has not returned within limit.
func returnsWithin[T any](t *testing.T, limit time.Duration, call func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- call() }()
	select {
	case v := <-done:
		return v
	case <-time.After(limit):
		t.Fatalf("still waiting after %s", limit)
		panic("unreachable")
	}
}

func TestAHungCredentialStoreEndsWithTheCommand(t *testing.T) {
	session := &Session{
		Stores: &Stores{Keyring: KeyringStore{Keyring: newHungKeyring(t)}, File: FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}},
		HTTP:   NewHTTPClient(),
		Now:    func() time.Time { return clock },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := returnsWithin(t, 5*time.Second, func() error {
		_, err := session.AccessToken(ctx, "https://api.costfluent.com")
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the command's deadline", err)
	}
}

func TestAHungCredentialStoreIsGivenUpOn(t *testing.T) {
	const api = "https://api.costfluent.com"
	s := KeyringStore{Keyring: newHungKeyring(t), Timeout: 50 * time.Millisecond}
	calls := map[string]func() error{
		"load":   func() error { _, err := s.Load(context.Background(), api); return err },
		"save":   func() error { return s.Save(context.Background(), credentialFor(api)) },
		"delete": func() error { return s.Delete(context.Background(), api) },
	}
	for name, call := range calls {
		err := returnsWithin(t, 5*time.Second, call)
		if !errors.Is(err, errKeyringTimeout) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrNotSignedIn) ||
			!strings.Contains(err.Error(), "costfluent login --insecure-storage") || !strings.Contains(err.Error(), "COSTFLUENT_API_TOKEN") {
			t.Errorf("%s: err = %v, want a timeout naming the ways out", name, err)
		}
	}

	// A command stopped, as by Ctrl-C, stops waiting at once, under the default bound too.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := returnsWithin(t, 5*time.Second, func() error {
		_, err := KeyringStore{Keyring: newHungKeyring(t)}.Load(ctx, api)
		return err
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, errKeyringTimeout) {
		t.Errorf("cancelled: err = %v", err)
	}
}
