package pool

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testHostKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " root@box"
}

func TestStaticSourceNormalizesHosts(t *testing.T) {
	key := testHostKey(t)
	source, err := NewStaticSource("", 0, "", []StaticHost{
		{Name: "box-1", Address: "10.0.0.5", HostKey: key + "\n"},
		{Name: "box-2", Address: "[2001:db8::1]:2222", HostKey: key},
		{Name: "box-3", Address: "2001:db8::2", HostKey: key},
		{Name: "box-4", Address: "ci-4.internal.example:2200", HostKey: key},
	})
	if err != nil {
		t.Fatal(err)
	}
	if facts := source.Facts(); facts.Billing != BillingOwned || facts.CanCreate {
		t.Fatalf("facts = %+v, want owned hosts that cannot be created", facts)
	}
	hosts, err := source.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 4 || hosts[0].ID != "box-1" || hosts[0].Address != "10.0.0.5:22" || hosts[1].Address != "[2001:db8::1]:2222" ||
		hosts[2].Address != "[2001:db8::2]:22" || hosts[3].Address != "ci-4.internal.example:2200" {
		t.Fatalf("hosts = %+v", hosts)
	}
	if strings.HasSuffix(hosts[0].HostKey, "root@box") || !strings.HasPrefix(hosts[0].HostKey, "ssh-ed25519 ") {
		t.Fatalf("host key = %q, want the bare authorized key", hosts[0].HostKey)
	}
	hosts[0].ID = "changed"
	if again, _ := source.List(context.Background()); again[0].ID != "box-1" {
		t.Fatal("List exposed the source's own slice")
	}
	if _, err := source.Create(context.Background()); err == nil {
		t.Fatal("a static source created a host")
	}
	if err := source.Delete(context.Background(), "box-1"); err == nil {
		t.Fatal("a static source deleted a host")
	}
}

func TestStaticSourceRejectsInvalidHosts(t *testing.T) {
	key := testHostKey(t)
	cases := map[string]struct {
		billing Billing
		hosts   []StaticHost
	}{
		"no hosts":        {hosts: nil},
		"unknown billing": {billing: "monthly", hosts: []StaticHost{{Name: "a", Address: "10.0.0.1", HostKey: key}}},
		"bad name":        {hosts: []StaticHost{{Name: "Box_1", Address: "10.0.0.1", HostKey: key}}},
		"duplicate":       {hosts: []StaticHost{{Name: "a", Address: "10.0.0.1", HostKey: key}, {Name: "a", Address: "10.0.0.2", HostKey: key}}},
		"bad address":     {hosts: []StaticHost{{Name: "a", Address: "10.0.0.1:22:22", HostKey: key}}},
		"bad port":        {hosts: []StaticHost{{Name: "a", Address: "10.0.0.1:99999", HostKey: key}}},
		"spaces":          {hosts: []StaticHost{{Name: "a", Address: "box -oProxyCommand=x", HostKey: key}}},
		"no host key":     {hosts: []StaticHost{{Name: "a", Address: "10.0.0.1"}}},
		"bad host key":    {hosts: []StaticHost{{Name: "a", Address: "10.0.0.1", HostKey: "ssh-ed25519 AAAA"}}},
	}
	for name, tc := range cases {
		if _, err := NewStaticSource(tc.billing, 0, "", tc.hosts); err == nil {
			t.Errorf("%s: NewStaticSource accepted invalid hosts", name)
		}
	}
}

func TestStaticPoolNeverReturnsItsHosts(t *testing.T) {
	source, err := NewStaticSource(BillingPerSecond, 0.02, "EUR", []StaticHost{{Name: "box", Address: "10.0.0.5", HostKey: testHostKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
	agent := newFakeAgent()
	agent.ready("box", 2)
	created, err := New(Config{
		Name: "box", ControllerID: "fleet", Image: testImage, Source: source, Agent: agent,
		MaxLifetime: 2 * time.Hour, Now: clock.Now, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{pool: created, agent: agent, clock: clock}
	h.step(t, time.Second)
	h.step(t, 3*time.Hour)
	status := created.Status()
	if len(status.Hosts) != 1 || status.Hosts[0].Phase != string(phaseReady) || status.ReturnedHosts != 0 {
		t.Fatalf("status = %+v, want the static host kept ready", status)
	}
}
