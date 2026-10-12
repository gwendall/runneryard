package pool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// StaticHost is a machine the operator prepared with "runneryard host install".
type StaticHost struct {
	Name    string `yaml:"name" json:"name"`
	Address string `yaml:"address" json:"address"`
	// HostKey is the host's SSH public key, such as the content of
	// /etc/ssh/ssh_host_ed25519_key.pub.
	HostKey string `yaml:"host_key" json:"host_key"`
}

// StaticSource serves a fixed list of hosts. It never creates or deletes a
// machine: the operator owns them.
type StaticSource struct {
	facts Facts
	hosts []Host
}

// NewStaticSource validates the hosts. Billing is usually owned; a rented
// machine paid by the month is owned too.
func NewStaticSource(billing Billing, hourlyPrice float64, currency string, hosts []StaticHost) (*StaticSource, error) {
	if billing == "" {
		billing = BillingOwned
	}
	switch billing {
	case BillingOwned, BillingPerSecond, BillingPerStartedHour:
	default:
		return nil, fmt.Errorf("unknown billing %q", billing)
	}
	if len(hosts) == 0 {
		return nil, errors.New("a static pool needs at least one host")
	}
	seen := make(map[string]struct{}, len(hosts))
	listed := make([]Host, 0, len(hosts))
	for _, host := range hosts {
		if !ValidName(host.Name) {
			return nil, fmt.Errorf("static host name %q must be lowercase letters, digits, and hyphens", host.Name)
		}
		if _, duplicate := seen[host.Name]; duplicate {
			return nil, fmt.Errorf("static host %q appears twice", host.Name)
		}
		seen[host.Name] = struct{}{}
		address, ok := staticAddress(host.Address)
		if !ok {
			return nil, fmt.Errorf("static host %s has an invalid address %q; use an IP or a DNS name, with an optional :port", host.Name, host.Address)
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(host.HostKey)))
		if err != nil {
			return nil, fmt.Errorf("static host %s needs its SSH host public key (ssh-keyscan or /etc/ssh/ssh_host_ed25519_key.pub): %w", host.Name, err)
		}
		listed = append(listed, Host{ID: host.Name, Address: address, HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), State: "running"})
	}
	return &StaticSource{facts: Facts{Billing: billing, HourlyPrice: hourlyPrice, Currency: currency}, hosts: listed}, nil
}

var dnsName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// staticAddress returns host:port for an IP or DNS name, defaulting to port 22.
func staticAddress(address string) (string, bool) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host, port = address, "22"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", false
	}
	if net.ParseIP(host) == nil && (len(host) > 253 || !dnsName.MatchString(host)) {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

func (s *StaticSource) Facts() Facts { return s.facts }

func (s *StaticSource) List(context.Context) ([]Host, error) {
	return append([]Host(nil), s.hosts...), nil
}

func (s *StaticSource) Create(context.Context, string) (Host, error) {
	return Host{}, errors.New("a static pool cannot create hosts")
}

func (s *StaticSource) Delete(context.Context, string) error {
	return errors.New("a static pool never deletes hosts")
}
