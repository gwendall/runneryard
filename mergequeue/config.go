package mergequeue

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RequiredCheck is one check the queue waits for before advancing the
// target branch. A check with no Paths is required for every stack; a
// check with Paths is only required when the stack touches a matching
// file, mirroring the path filter on the workflow that reports it.
type RequiredCheck struct {
	Name  string
	Paths []string
}

// Config is the queue's configuration, read from ConfigPath.
type Config struct {
	Target         string
	Label          string
	BatchSize      int
	RequiredChecks []RequiredCheck
	CheckTimeout   time.Duration
}

// WithDefaults fills in the documented defaults for any zero field.
func (c Config) WithDefaults() Config {
	if c.Target == "" {
		c.Target = "main"
	}
	if c.Label == "" {
		c.Label = DefaultLabel
	}
	if c.BatchSize < 1 {
		c.BatchSize = 3
	}
	if c.CheckTimeout <= 0 {
		c.CheckTimeout = 45 * time.Minute
	}
	return c
}

type configFile struct {
	Target         string `yaml:"target"`
	Label          string `yaml:"label"`
	BatchSize      int    `yaml:"batch_size"`
	CheckTimeout   string `yaml:"check_timeout"`
	RequiredChecks []struct {
		Name  string   `yaml:"name"`
		Paths []string `yaml:"paths"`
	} `yaml:"required_checks"`
}

// maxBatchSize bounds how many pull requests one stack holds, so one tick
// stays within a few dozen API calls.
const maxBatchSize = 10

// ParseConfig reads the queue configuration from data.
func ParseConfig(data []byte) (Config, error) {
	var file configFile
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return Config{}, fmt.Errorf("parse merge queue config: %w", err)
	}
	if file.BatchSize < 0 || file.BatchSize > maxBatchSize {
		return Config{}, fmt.Errorf("parse merge queue config: batch_size must be between 1 and %d", maxBatchSize)
	}
	config := Config{Target: strings.TrimSpace(file.Target), Label: strings.TrimSpace(file.Label), BatchSize: file.BatchSize}
	if len(file.RequiredChecks) == 0 {
		return Config{}, fmt.Errorf("parse merge queue config: required_checks must list at least one check")
	}
	for _, check := range file.RequiredChecks {
		if strings.TrimSpace(check.Name) == "" {
			return Config{}, fmt.Errorf("parse merge queue config: a required check has no name")
		}
		config.RequiredChecks = append(config.RequiredChecks, RequiredCheck{Name: check.Name, Paths: check.Paths})
	}
	if file.CheckTimeout != "" {
		timeout, err := time.ParseDuration(file.CheckTimeout)
		if err != nil || timeout <= 0 {
			return Config{}, fmt.Errorf("parse merge queue config: invalid check_timeout %q", file.CheckTimeout)
		}
		config.CheckTimeout = timeout
	}
	return config.WithDefaults(), nil
}
