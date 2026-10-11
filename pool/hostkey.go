package pool

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ControllerKey is the controller's SSH identity toward its hosts.
type ControllerKey struct {
	private ed25519.PrivateKey
	signer  ssh.Signer
}

// ParseControllerKey reads an OpenSSH ed25519 private key without a
// passphrase (ssh-keygen -t ed25519 -N "").
func ParseControllerKey(contents []byte) (*ControllerKey, error) {
	raw, err := ssh.ParseRawPrivateKey(contents)
	if err != nil {
		return nil, fmt.Errorf("parse pool SSH key: %w", err)
	}
	var private ed25519.PrivateKey
	switch key := raw.(type) {
	case ed25519.PrivateKey:
		private = key
	case *ed25519.PrivateKey:
		private = *key
	default:
		return nil, errors.New("the pool SSH key must be an ed25519 key (ssh-keygen -t ed25519)")
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, fmt.Errorf("load pool SSH key: %w", err)
	}
	return &ControllerKey{private: private, signer: signer}, nil
}

// Signer authenticates the controller to host agents.
func (k *ControllerKey) Signer() ssh.Signer { return k.signer }

// AuthorizedKey is the controller's public key in authorized_keys format.
func (k *ControllerKey) AuthorizedKey() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k.signer.PublicKey())))
}

// HostKey returns the SSH host key for the host named name, as an OpenSSH
// private key and its public authorized_keys line. It is derived from the
// controller key, so the controller can pin the key of every host it created
// without storing anything, across restarts.
func (k *ControllerKey) HostKey(name string) ([]byte, string, error) {
	mac := hmac.New(sha256.New, k.private.Seed())
	mac.Write([]byte("runneryard host key v1|" + name))
	hostKey := ed25519.NewKeyFromSeed(mac.Sum(nil))
	block, err := ssh.MarshalPrivateKey(hostKey, "runneryard host "+name)
	if err != nil {
		return nil, "", fmt.Errorf("encode host key: %w", err)
	}
	public, err := ssh.NewPublicKey(hostKey.Public())
	if err != nil {
		return nil, "", fmt.Errorf("encode host public key: %w", err)
	}
	return pem.EncodeToMemory(block), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public))), nil
}
