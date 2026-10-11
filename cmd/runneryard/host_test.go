package main

import (
	"strings"
	"testing"
)

func TestHostBootRefusesToStartWithoutAPrivateAddress(t *testing.T) {
	for _, privateIP := range []string{"", "fdaa::zz", "not-an-ip", "10.0.0.2:2222", "::", "0.0.0.0"} {
		if listen, err := bootListen(privateIP); err == nil || !strings.Contains(err.Error(), "FLY_PRIVATE_IP") {
			t.Errorf("bootListen(%q) = %q, %v; want a refusal naming FLY_PRIVATE_IP", privateIP, listen, err)
		}
		t.Setenv("FLY_PRIVATE_IP", privateIP)
		// The refusal comes before anything else boot does, so the test needs
		// neither root nor Docker.
		if err := runHostBoot(nil); err == nil || !strings.Contains(err.Error(), "FLY_PRIVATE_IP") {
			t.Errorf("host boot with FLY_PRIVATE_IP=%q: %v; want a refusal naming FLY_PRIVATE_IP", privateIP, err)
		}
	}
	// No flag chooses another address, such as every interface.
	t.Setenv("FLY_PRIVATE_IP", "fdaa:0:1::2")
	if err := runHostBoot([]string{"--listen", ":2222"}); err == nil {
		t.Fatal("host boot accepted --listen")
	}
}

func TestHostBootListensOnThePrivateAddress(t *testing.T) {
	for privateIP, want := range map[string]string{
		"fdaa:0:1:a7b::2": "[fdaa:0:1:a7b::2]:2222",
		"10.0.0.2":        "10.0.0.2:2222",
	} {
		listen, err := bootListen(privateIP)
		if err != nil || listen != want {
			t.Errorf("bootListen(%q) = %q, %v; want %q", privateIP, listen, err, want)
		}
	}
}
