package pool_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/gwendall/runneryard/pool"
	"golang.org/x/crypto/ssh"
)

// controllerKeyPEM returns a fresh OpenSSH ed25519 private key, as
// "ssh-keygen -t ed25519 -N ”" writes it.
func controllerKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "runneryard controller")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

func generateControllerKey(t *testing.T) *pool.ControllerKey {
	t.Helper()
	key, err := pool.ParseControllerKey(controllerKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// hostKeyPair returns the parsed private and pinned public halves of a
// derived host key.
func hostKeyPair(t *testing.T, controller *pool.ControllerKey, name string) (ssh.Signer, ssh.PublicKey, string) {
	t.Helper()
	private, public, err := controller.HostKey(name)
	if err != nil {
		t.Fatalf("HostKey(%q) error = %v", name, err)
	}
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		t.Fatalf("HostKey(%q) private half does not parse: %v", name, err)
	}
	pinned, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(public))
	if err != nil || len(rest) != 0 {
		t.Fatalf("HostKey(%q) public half %q does not parse: %v", name, public, err)
	}
	return signer, pinned, public
}

func TestHostKeyIsDerivedFromTheControllerKeyAndName(t *testing.T) {
	t.Parallel()
	contents := controllerKeyPEM(t)
	first, err := pool.ParseControllerKey(contents)
	if err != nil {
		t.Fatal(err)
	}
	// A restarted controller reloads the same file and must pin the same keys.
	restarted, err := pool.ParseControllerKey(contents)
	if err != nil {
		t.Fatal(err)
	}
	signer, _, public := hostKeyPair(t, first, "hetzner-123")
	againSigner, _, againPublic := hostKeyPair(t, restarted, "hetzner-123")
	if public != againPublic || !bytes.Equal(signer.PublicKey().Marshal(), againSigner.PublicKey().Marshal()) {
		t.Fatalf("host key changed across reloads: %q then %q", public, againPublic)
	}
	if !strings.HasPrefix(public, "ssh-ed25519 ") || strings.ContainsAny(public, "\r\n") {
		t.Fatalf("public host key = %q, want one ssh-ed25519 line", public)
	}
	_, _, otherName := hostKeyPair(t, first, "hetzner-124")
	_, _, otherController := hostKeyPair(t, generateControllerKey(t), "hetzner-123")
	if otherName == public || otherController == public {
		t.Fatalf("host keys collide: name %q, controller %q, base %q", otherName, otherController, public)
	}
	if public == first.AuthorizedKey() {
		t.Fatal("the host key is the controller key itself")
	}
}

func TestHostKeyPrivateHalfMatchesThePinnedKey(t *testing.T) {
	t.Parallel()
	signer, pinned, _ := hostKeyPair(t, generateControllerKey(t), "h1")
	if !bytes.Equal(signer.PublicKey().Marshal(), pinned.Marshal()) {
		t.Fatal("the host's private key does not match the key the controller pins")
	}
	data := []byte("session identifier")
	signature, err := signer.Sign(rand.Reader, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned.Verify(data, signature); err != nil {
		t.Fatalf("pinned key rejects the host's signature: %v", err)
	}
}

func TestParseControllerKeyAcceptsOnlyAnUnencryptedEd25519Key(t *testing.T) {
	t.Parallel()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := pool.ParseControllerKey(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatalf("ParseControllerKey(ed25519) error = %v", err)
	}
	public, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))); key.AuthorizedKey() != want {
		t.Fatalf("AuthorizedKey() = %q, want %q", key.AuthorizedKey(), want)
	}
	if !bytes.Equal(key.Signer().PublicKey().Marshal(), public.Marshal()) {
		t.Fatal("Signer() does not sign with the parsed key")
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rejected := map[string][]byte{
		"rsa":           marshalPrivate(t, rsaKey),
		"ecdsa":         marshalPrivate(t, ecdsaKey),
		"encrypted":     encryptedKeyPEM(public),
		"public key":    ssh.MarshalAuthorizedKey(public),
		"garbage":       []byte("not a key"),
		"empty":         nil,
		"truncated pem": pem.EncodeToMemory(block)[:100],
	}
	for name, contents := range rejected {
		if _, err := pool.ParseControllerKey(contents); err == nil {
			t.Errorf("ParseControllerKey(%s) = nil error", name)
		}
	}
	var missing *ssh.PassphraseMissingError
	if _, err := pool.ParseControllerKey(encryptedKeyPEM(public)); !errors.As(err, &missing) {
		t.Errorf("ParseControllerKey(encrypted) error = %v, want a missing passphrase", err)
	}
	if _, err := pool.ParseControllerKey(marshalPrivate(t, rsaKey)); err == nil || !strings.Contains(err.Error(), "ed25519") {
		t.Errorf("ParseControllerKey(rsa) error = %v, want it to ask for ed25519", err)
	}
}

// encryptedKeyPEM has the layout "ssh-keygen -t ed25519 -N passphrase"
// writes. The parser refuses it before any decryption, so the private block is
// filler; ssh.MarshalPrivateKeyWithPassphrase would spend seconds in bcrypt
// under the race detector.
func encryptedKeyPEM(public ssh.PublicKey) []byte {
	kdfOptions := ssh.Marshal(struct {
		Salt   []byte
		Rounds uint32
	}{Salt: make([]byte, 16), Rounds: 16})
	body := ssh.Marshal(struct {
		CipherName   string
		KdfName      string
		KdfOpts      string
		NumKeys      uint32
		PubKey       []byte
		PrivKeyBlock []byte
	}{"aes256-ctr", "bcrypt", string(kdfOptions), 1, public.Marshal(), make([]byte, 160)})
	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: append([]byte("openssh-key-v1\x00"), body...)})
}

func marshalPrivate(t *testing.T, key any) []byte {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}
