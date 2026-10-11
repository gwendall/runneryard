package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gwendall/runneryard/githubapp"
	"github.com/gwendall/runneryard/mergequeue"
)

func TestMergeQueueProfileRequestsTheDocumentedPermissionsOnly(t *testing.T) {
	profile := mergeQueueProfile(false)
	want := map[string]string{"actions": "write", "checks": "read", "contents": "write", "issues": "write", "metadata": "read", "pull_requests": "write"}
	if !reflect.DeepEqual(profile.Permissions, want) {
		t.Fatalf("permissions = %v, want %v", profile.Permissions, want)
	}
	if got := mergeQueueProfile(true).Permissions["workflows"]; got != "write" {
		t.Fatalf("--workflows permission = %q, want write", got)
	}
	// The random suffix is 8 hex characters after a space; GitHub caps App
	// names at 34.
	if length := len(profile.Name) + 1 + 8; length > 34 {
		t.Fatalf("App name would be %d characters", length)
	}
	if strings.Contains(strings.ToLower(profile.Name), "runner app") || profile.Name == githubapp.RunnerProfile(githubapp.Target{Owner: "octo", Repository: "repo"}).Name {
		t.Fatalf("queue App name %q must differ from the runner App's", profile.Name)
	}
}

func TestMergeQueueTargetRefusesAnAccountWithTheQueuesReason(t *testing.T) {
	for _, url := range []string{"https://github.com/octo", "https://github.com/octo/"} {
		if _, err := mergeQueueTarget(context.Background(), authOptions{githubURL: url, ownerKind: "auto"}); !errors.Is(err, errMergeQueueNeedsRepository) {
			t.Fatalf("%s: err = %v", url, err)
		}
	}
}

type setupHarness struct {
	t          *testing.T
	key        string
	gh         []string
	ghFails    bool
	bootstraps int
	reuses     int
	reused     githubapp.Credentials
}

func newSetupHarness(t *testing.T) *setupHarness {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &setupHarness{t: t, key: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
}

func (h *setupHarness) setup() mergeQueueSetup {
	store := func(ctx context.Context, options githubapp.Options, credentials githubapp.Credentials) (githubapp.Result, error) {
		if options.Profile == nil || !reflect.DeepEqual(options.Profile.Permissions, mergequeue.AppPermissions(false)) {
			h.t.Fatalf("setup did not pass the queue profile: %+v", options.Profile)
		}
		credentials.InstallationID = 77
		if err := options.Sink.Store(ctx, credentials); err != nil {
			return githubapp.Result{}, err
		}
		return githubapp.Result{AppURL: "https://github.com/apps/runneryard-merge-queue-abcd", Slug: "runneryard-merge-queue-abcd", InstallationID: 77, SinkDescription: options.Sink.Description()}, nil
	}
	return mergeQueueSetup{
		target: func(_ context.Context, options authOptions) (githubapp.Target, error) {
			parts := strings.Split(strings.TrimPrefix(options.githubURL, "https://github.com/"), "/")
			target := githubapp.Target{ConfigURL: options.githubURL, Owner: parts[0], OwnerKind: githubapp.OwnerUser}
			if len(parts) == 2 {
				target.Repository = parts[1]
			}
			return target, nil
		},
		runGH: func(_ context.Context, args []string, stdin []byte) error {
			if h.ghFails {
				return errors.New("exit status 1")
			}
			h.gh = append(h.gh, strings.Join(args, " "))
			return nil
		},
		bootstrap: func(ctx context.Context, options githubapp.Options) (githubapp.Result, error) {
			h.bootstraps++
			return store(ctx, options, githubapp.Credentials{ClientID: "Iv1.queue", PrivateKey: h.key})
		},
		reuse: func(ctx context.Context, options githubapp.Options, credentials githubapp.Credentials) (githubapp.Result, error) {
			h.reuses++
			h.reused = credentials
			return store(ctx, options, credentials)
		},
		readKey: func(string) (string, error) { return h.key, nil },
	}
}

func TestMergeQueueSetupStoresTheAppWithoutPrintingItsKey(t *testing.T) {
	h := newSetupHarness(t)
	var output strings.Builder
	if err := runMergeQueueSetup([]string{"--github", "https://github.com/octo/repo", "--no-browser"}, &output, h.setup()); err != nil {
		t.Fatal(err)
	}
	if h.bootstraps != 1 || h.reuses != 0 {
		t.Fatalf("bootstraps = %d, reuses = %d", h.bootstraps, h.reuses)
	}
	want := []string{
		"api --silent repos/octo/repo/actions/secrets/public-key",
		"secret set MERGE_QUEUE_APP_PRIVATE_KEY --app actions --repo octo/repo",
		"variable set MERGE_QUEUE_APP_CLIENT_ID --repo octo/repo --body Iv1.queue",
	}
	if !reflect.DeepEqual(h.gh, want) {
		t.Fatalf("gh calls = %q, want %q", h.gh, want)
	}
	text := output.String()
	if strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, strings.TrimSpace(h.key)[40:80]) {
		t.Fatalf("output contains the private key:\n%s", text)
	}
	if !strings.Contains(text, "runneryard-merge-queue-abcd[bot]") || !strings.Contains(text, "gh variable delete MERGE_QUEUE_APP_CLIENT_ID --repo octo/repo") {
		t.Fatalf("output lacks the bot login or the way back:\n%s", text)
	}
}

func TestMergeQueueSetupReusesAnExistingApp(t *testing.T) {
	h := newSetupHarness(t)
	var output strings.Builder
	args := []string{"--github", "https://github.com/octo/other", "--client-id", "Iv1.queue", "--private-key-file", "key.pem"}
	if err := runMergeQueueSetup(args, &output, h.setup()); err != nil {
		t.Fatal(err)
	}
	if h.bootstraps != 0 || h.reuses != 1 || h.reused.ClientID != "Iv1.queue" || h.reused.PrivateKey != h.key {
		t.Fatalf("bootstraps = %d, reuses = %d, reused client %q", h.bootstraps, h.reuses, h.reused.ClientID)
	}
	if !strings.Contains(strings.Join(h.gh, "\n"), "--repo octo/other") {
		t.Fatalf("gh calls = %q", h.gh)
	}
}

func TestMergeQueueSetupCreatesNothingItCannotStore(t *testing.T) {
	for name, test := range map[string]struct {
		args    []string
		ghFails bool
		want    string
	}{
		"gh cannot manage secrets": {args: []string{"--github", "https://github.com/octo/repo"}, ghFails: true, want: "gh cannot manage the Actions secrets of octo/repo"},
		"organization URL":         {args: []string{"--github", "https://github.com/octo"}, want: "serves one repository"},
		"client ID without key":    {args: []string{"--github", "https://github.com/octo/repo", "--client-id", "Iv1.queue"}, want: "go together"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSetupHarness(t)
			h.ghFails = test.ghFails
			var output strings.Builder
			err := runMergeQueueSetup(test.args, &output, h.setup())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
			if h.bootstraps != 0 || h.reuses != 0 {
				t.Fatal("setup created or reused an App it could not store")
			}
		})
	}
}
