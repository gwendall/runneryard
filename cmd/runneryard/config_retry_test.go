package main

import "testing"

func TestConfigUsesRetryDefaults(t *testing.T) {
	setValidEnv(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderRetries != 5 || cfg.ProviderRateLimit != 5 || cfg.GitHubAPIRateLimit != 10 {
		t.Fatalf("unexpected retry defaults: attempts=%d provider=%v github=%v", cfg.ProviderRetries, cfg.ProviderRateLimit, cfg.GitHubAPIRateLimit)
	}
	policy := cfg.retryPolicy()
	if policy.Attempts != 5 || policy.Rate != 5 || policy.Burst != 10 {
		t.Fatalf("unexpected provider policy %#v", policy)
	}
}

func TestConfigRejectsOutOfRangeRetrySettings(t *testing.T) {
	cases := map[string]string{
		"PROVIDER_RETRY_ATTEMPTS": "0",
		"PROVIDER_RATE_LIMIT":     "0",
		"GITHUB_API_RATE_LIMIT":   "1000",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(name, value)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("expected %s=%s to be rejected", name, value)
			}
		})
	}
}

func TestConfigRejectsNonNumericRateLimit(t *testing.T) {
	setValidEnv(t)
	t.Setenv("PROVIDER_RATE_LIMIT", "fast")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected a non-numeric rate limit to be rejected")
	}
}
