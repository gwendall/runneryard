package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gwendall/runneryard/controller"
)

func TestEphemeralScaleSetNameIsLoadedAndValidated(t *testing.T) {
	setValidEnv(t)
	t.Setenv("SCALE_SET_NAME", "acme-linux")
	t.Setenv(poolsFileEnv, "")

	t.Setenv("EPHEMERAL_SCALE_SET_NAME", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if name := cfg.controllerConfig(nil).EphemeralScaleSetName; name != "" {
		t.Fatalf("unset, the controller must serve one scale set, got %q", name)
	}

	t.Setenv("EPHEMERAL_SCALE_SET_NAME", " acme-linux ")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "must differ from SCALE_SET_NAME") {
		t.Fatalf("a second scale set with the main one's name must be refused, got %v", err)
	}

	t.Setenv("EPHEMERAL_SCALE_SET_NAME", " acme-linux-ephemeral ")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if name := cfg.controllerConfig(nil).EphemeralScaleSetName; name != "acme-linux-ephemeral" {
		t.Fatalf("controller scale set = %q", name)
	}
	compute, ephemeral, err := cfg.computes(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if compute == nil || ephemeral != nil {
		t.Fatalf("without pools every worker is already ephemeral: compute=%T ephemeral=%T", compute, ephemeral)
	}
}

func TestEphemeralScaleSetNeedsAnEphemeralTier(t *testing.T) {
	setValidEnv(t)
	t.Setenv("COMPUTE_PROVIDER", computeProviderNone)
	t.Setenv("HCLOUD_TOKEN", "hcloud-token")
	t.Setenv("HCLOUD_API_BASE_URL", "http://127.0.0.1:9")
	t.Setenv(poolSSHKeyFileEnv, "")
	path := filepath.Join(t.TempDir(), "pools.yaml")
	contents := poolsYAML("trust: private\nssh_key_file: "+writePoolKey(t)+"\n", hetznerPool("fast"))
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(poolsFileEnv, path)
	t.Setenv("EPHEMERAL_SCALE_SET_NAME", "acme-linux-ephemeral")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "COMPUTE_PROVIDER=none has none") {
		t.Fatalf("a pools-only fleet cannot serve the ephemeral scale set, got %v", err)
	}
}

func TestEphemeralScaleSetLaunchesOnTheEphemeralTierAlone(t *testing.T) {
	t.Setenv(poolSSHKeyFileEnv, "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	top := "trust: private\nssh_key_file: " + writePoolKey(t) + "\n"
	for name, test := range map[string]struct {
		placement    string
		wantTiers    []string
		wantDraining []string
	}{
		"ephemeral placed last":   {wantTiers: []string{"fast", ephemeralTier}},
		"ephemeral out of order":  {placement: "placement: [fast]\n", wantTiers: []string{"fast"}, wantDraining: []string{ephemeralTier}},
		"only the ephemeral tier": {placement: "placement: [ephemeral]\n", wantTiers: []string{ephemeralTier}, wantDraining: []string{"fast"}},
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := loadPoolsYAML(t, poolsYAML(top+test.placement, hetznerPool("fast")))
			if err != nil {
				t.Fatal(err)
			}
			cfg := poolsAppConfig()
			cfg.HetznerServerType, cfg.HetznerServerImage = "cpx32", defaultHetznerImage
			if err := parsed.validate(cfg); err != nil {
				t.Fatal(err)
			}
			cfg.Pools = parsed

			compute, ephemeral, err := cfg.computes(logger)
			if err != nil {
				t.Fatal(err)
			}
			if ephemeral != nil {
				t.Fatalf("without the option there is no second launcher, got %T", ephemeral)
			}
			status := placementStatus(t, compute)
			if !slices.Equal(status.Tiers, test.wantTiers) || !slices.Equal(status.Draining, test.wantDraining) {
				t.Fatalf("the option must leave the placement as it was: tiers = %v draining = %v", status.Tiers, status.Draining)
			}

			cfg.EphemeralScaleSetName = "acme-linux-ephemeral"
			compute, ephemeral, err = cfg.computes(logger)
			if err != nil {
				t.Fatal(err)
			}
			status = placementStatus(t, compute)
			if !slices.Equal(status.Tiers, test.wantTiers) || !slices.Equal(status.Draining, test.wantDraining) {
				t.Fatalf("the main scale set's placement must not change: tiers = %v draining = %v", status.Tiers, status.Draining)
			}
			// The main placement inventories and destroys the ephemeral tier,
			// placed or draining, so it owns every worker the second scale
			// set's launcher starts.
			if !slices.Contains(slices.Concat(status.Tiers, status.Draining), ephemeralTier) {
				t.Fatalf("the main placement must cover the ephemeral tier: %#v", status)
			}
			direct := placementStatus(t, ephemeral)
			if !slices.Equal(direct.Tiers, []string{ephemeralTier}) || len(direct.Draining) != 0 || len(direct.Pools) != 0 {
				t.Fatalf("the second scale set must launch on the ephemeral tier alone: %#v", direct)
			}
		})
	}
}

func TestWriteFleetStatusListsEachScaleSet(t *testing.T) {
	status := controller.FleetStatus{
		SchemaVersion: 2,
		GitHub: controller.GitHubStatus{ScaleSet: "acme-linux", AssignedJobs: 4, DesiredWorkers: 4, ScaleSets: []controller.ScaleSetStatus{
			{Name: "acme-linux", AssignedJobs: 3, DesiredWorkers: 3, Workers: 2},
			{Name: "acme-linux-ephemeral", AssignedJobs: 1, DesiredWorkers: 1, Workers: 1},
		}},
	}
	var output bytes.Buffer
	if err := writeFleetStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Scale set   acme-linux  desired 3 from 3 assigned job(s)  2 worker(s)",
		"Scale set   acme-linux-ephemeral  desired 1 from 1 assigned job(s)  1 worker(s)",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("status output missing %q:\n%s", expected, output.String())
		}
	}
	if strings.Count(output.String(), "Scale set ") != 2 {
		t.Fatalf("each scale set must be listed once:\n%s", output.String())
	}
}
