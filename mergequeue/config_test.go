package mergequeue

import (
	"os"
	"testing"
	"time"
)

func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile("../docs/examples/merge-queue-config.yml")
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if config.Label != DefaultLabel || len(config.RequiredChecks) != 2 {
		t.Fatalf("example config = %#v", config)
	}
}

func TestParseConfigAppliesDefaults(t *testing.T) {
	config, err := ParseConfig([]byte(`
required_checks:
  - name: unit-tests
  - name: integration-tests
    paths: ["services/api/**"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if config.Target != "main" || config.Label != "queue" || config.BatchSize != 3 || config.CheckTimeout != 45*time.Minute {
		t.Fatalf("unexpected defaults: %#v", config)
	}
	if len(config.RequiredChecks) != 2 || config.RequiredChecks[1].Paths[0] != "services/api/**" {
		t.Fatalf("unexpected checks: %#v", config.RequiredChecks)
	}
}

func TestParseConfigReadsEveryKey(t *testing.T) {
	config, err := ParseConfig([]byte(`
target: trunk
label: ship-it
batch_size: 5
check_timeout: 20m
required_checks: [{name: ci}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if config.Target != "trunk" || config.Label != "ship-it" || config.BatchSize != 5 || config.CheckTimeout != 20*time.Minute {
		t.Fatalf("config = %#v", config)
	}
}

func TestParseConfigRejectsInvalidFiles(t *testing.T) {
	for name, data := range map[string]string{
		"no required checks":    `target: main`,
		"a check with no name":  `required_checks: [{paths: ["x"]}]`,
		"an invalid timeout":    "required_checks: [{name: x}]\ncheck_timeout: not-a-duration",
		"a negative timeout":    "required_checks: [{name: x}]\ncheck_timeout: -5m",
		"a batch size too big":  "required_checks: [{name: x}]\nbatch_size: 11",
		"a negative batch size": "required_checks: [{name: x}]\nbatch_size: -1",
		"an unknown key":        "required_checks: [{name: x}]\nbatchsize: 2",
	} {
		if _, err := ParseConfig([]byte(data)); err == nil {
			t.Errorf("%s: ParseConfig accepted %q", name, data)
		}
	}
}
