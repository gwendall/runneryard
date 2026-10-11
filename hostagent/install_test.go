package hostagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"slices"
	"strings"
	"testing"

	"github.com/gwendall/runneryard/pool"
	"golang.org/x/crypto/ssh"
)

func newPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func authorizedText(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func TestAuthorizedKeysLineForcesTheAgentCommand(t *testing.T) {
	t.Parallel()
	key := newPublicKey(t)
	want := `restrict,command="/usr/local/bin/runneryard host" ` + authorizedText(key)
	for name, input := range map[string]string{
		"bare key":                   authorizedText(key),
		"key with a comment":         authorizedText(key) + " controller@runneryard",
		"key with surrounding space": "  " + authorizedText(key) + "\n\n",
	} {
		line, err := AuthorizedKeysLine(input)
		if err != nil {
			t.Fatalf("%s: AuthorizedKeysLine() error = %v", name, err)
		}
		if line != want {
			t.Fatalf("%s: line = %q, want %q", name, line, want)
		}
		parsed, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			t.Fatalf("%s: sshd could not parse %q: %v", name, line, err)
		}
		if !bytes.Equal(parsed.Marshal(), key.Marshal()) || len(rest) != 0 {
			t.Fatalf("%s: parsed key differs or trailing data %q", name, rest)
		}
		if wantOptions := []string{"restrict", `command="/usr/local/bin/runneryard host"`}; !slices.Equal(options, wantOptions) {
			t.Fatalf("%s: options = %q, want %q", name, options, wantOptions)
		}
	}
}

func TestAuthorizedKeysLineAcceptsTheControllerKey(t *testing.T) {
	t.Parallel()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "controller")
	if err != nil {
		t.Fatal(err)
	}
	controller, err := pool.ParseControllerKey(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatal(err)
	}
	line, err := AuthorizedKeysLine(controller.AuthorizedKey())
	if err != nil {
		t.Fatalf("AuthorizedKeysLine(controller key) error = %v", err)
	}
	if !strings.HasSuffix(line, " "+controller.AuthorizedKey()) {
		t.Fatalf("line = %q, want it to end with the controller key", line)
	}
}

func TestAuthorizedKeysLineRejectsAnythingButOnePlainKey(t *testing.T) {
	t.Parallel()
	key := authorizedText(newPublicKey(t))
	other := authorizedText(newPublicKey(t))
	for name, input := range map[string]string{
		"empty":                   "",
		"garbage":                 "not a key",
		"truncated key":           strings.TrimSuffix(key, key[len(key)-10:]),
		"private key text":        "-----BEGIN OPENSSH PRIVATE KEY-----",
		"forced command option":   `command="/bin/sh" ` + key,
		"source restriction":      `from="10.0.0.1" ` + key,
		"permissive option":       "no-pty " + key,
		"two keys":                key + "\n" + other,
		"key then garbage":        key + "\nnot a key",
		"key then a comment line": key + "\n# second line",
	} {
		if line, err := AuthorizedKeysLine(input); err == nil {
			t.Errorf("%s: AuthorizedKeysLine() = %q, want an error", name, line)
		}
	}
}
