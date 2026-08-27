package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRouteGitHub struct {
	exists         bool
	value          string
	repoAccessible bool
	getError       error
	setError       error
	deleteError    error
	writes         int
	lastStdin      string
}

func (fake *fakeRouteGitHub) run(_ context.Context, arguments []string, stdin []byte) ([]byte, error) {
	if len(arguments) < 2 {
		return nil, errors.New("bad command")
	}
	if arguments[0] == "repo" && arguments[1] == "view" {
		if !fake.repoAccessible {
			return []byte("HTTP 404"), errors.New("repository not found")
		}
		return []byte("widgets\n"), nil
	}
	if arguments[0] != "variable" {
		return nil, errors.New("unexpected command")
	}
	name := arguments[2]
	switch arguments[1] {
	case "get":
		if fake.getError != nil {
			return []byte("HTTP 403"), fake.getError
		}
		if !fake.exists {
			return []byte("variable " + name + " was not found\n"), errors.New("not found")
		}
		return []byte(fake.value + "\n"), nil
	case "set":
		if fake.setError != nil {
			return []byte("HTTP 500"), fake.setError
		}
		fake.writes++
		fake.exists = true
		fake.value = string(stdin)
		fake.lastStdin = string(stdin)
		return nil, nil
	case "delete":
		if fake.deleteError != nil {
			return []byte("HTTP 500"), fake.deleteError
		}
		fake.writes++
		fake.exists = false
		fake.value = ""
		return nil, nil
	default:
		return nil, errors.New("unexpected variable command")
	}
}

func TestRouteStatusShowsHostedFallbackForMissingVariable(t *testing.T) {
	fake := &fakeRouteGitHub{repoAccessible: true}
	var output bytes.Buffer
	if err := runRouteWith([]string{"status", "--github", "https://github.com/acme/widgets"}, fake.run, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "ubuntu-latest") || fake.writes != 0 {
		t.Fatalf("output=%q writes=%d", output.String(), fake.writes)
	}
}

func TestRouteEnableRequiresCanaryAndUsesStdin(t *testing.T) {
	fake := &fakeRouteGitHub{repoAccessible: true}
	arguments := []string{"enable", "--github", "https://github.com/acme/widgets", "--label", "acme-linux"}
	if err := runRouteWith(arguments, fake.run, &bytes.Buffer{}); err == nil {
		t.Fatal("expected canary confirmation requirement")
	}
	arguments = append(arguments, "--confirm-canary")
	var output bytes.Buffer
	if err := runRouteWith(arguments, fake.run, &output); err != nil {
		t.Fatal(err)
	}
	if fake.writes != 1 || fake.lastStdin != "acme-linux" || !strings.Contains(output.String(), "now routes") {
		t.Fatalf("fake=%#v output=%q", fake, output.String())
	}
	if err := runRouteWith(arguments, fake.run, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if fake.writes != 1 {
		t.Fatalf("idempotent enable performed %d writes", fake.writes)
	}
}

func TestRouteDryRunDoesNotMutate(t *testing.T) {
	fake := &fakeRouteGitHub{repoAccessible: true}
	var output bytes.Buffer
	if err := runRouteWith([]string{
		"enable", "--github", "https://github.com/acme/widgets", "--label", "acme-linux", "--dry-run",
	}, fake.run, &output); err != nil {
		t.Fatal(err)
	}
	if fake.writes != 0 || !strings.Contains(output.String(), "would route") {
		t.Fatalf("output=%q writes=%d", output.String(), fake.writes)
	}
}

func TestRouteDisableDeletesAndVerifies(t *testing.T) {
	fake := &fakeRouteGitHub{exists: true, value: "acme-linux", repoAccessible: true}
	var output bytes.Buffer
	if err := runRouteWith([]string{"disable", "--github", "https://github.com/acme/widgets"}, fake.run, &output); err != nil {
		t.Fatal(err)
	}
	if fake.exists || fake.writes != 1 || !strings.Contains(output.String(), "ubuntu-latest") {
		t.Fatalf("fake=%#v output=%q", fake, output.String())
	}
	if err := runRouteWith([]string{"disable", "--github", "https://github.com/acme/widgets"}, fake.run, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if fake.writes != 1 {
		t.Fatalf("idempotent disable performed %d writes", fake.writes)
	}
}

func TestRouteRejectsUnsafeTargetsVariablesAndLabels(t *testing.T) {
	for name, arguments := range map[string][]string{
		"organization": {"status", "--github", "https://github.com/acme"},
		"query":        {"status", "--github", "https://github.com/acme/widgets?x=1"},
		"variable":     {"status", "--github", "https://github.com/acme/widgets", "--variable", "BAD-NAME"},
		"hosted label": {"enable", "--github", "https://github.com/acme/widgets", "--label", "ubuntu-latest", "--confirm-canary"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runRouteWith(arguments, (&fakeRouteGitHub{repoAccessible: true}).run, &bytes.Buffer{}); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestRouteDistinguishesUnavailableRepositoryAndReadFailure(t *testing.T) {
	for name, fake := range map[string]*fakeRouteGitHub{
		"repository": {repoAccessible: false},
		"permission": {repoAccessible: true, getError: errors.New("forbidden")},
	} {
		t.Run(name, func(t *testing.T) {
			err := runRouteWith([]string{"status", "--github", "https://github.com/acme/widgets"}, fake.run, &bytes.Buffer{})
			if err == nil {
				t.Fatal("expected access failure")
			}
		})
	}
}

func TestRouteDoesNotClaimSuccessAfterPartialFailure(t *testing.T) {
	tests := map[string]struct {
		arguments []string
		fake      *fakeRouteGitHub
	}{
		"enable": {
			arguments: []string{"enable", "--github", "https://github.com/acme/widgets", "--label", "acme-linux", "--confirm-canary"},
			fake:      &fakeRouteGitHub{repoAccessible: true, setError: errors.New("write failed")},
		},
		"disable": {
			arguments: []string{"disable", "--github", "https://github.com/acme/widgets"},
			fake:      &fakeRouteGitHub{repoAccessible: true, exists: true, value: "acme-linux", deleteError: errors.New("delete failed")},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := runRouteWith(test.arguments, test.fake.run, &output); err == nil {
				t.Fatal("expected mutation failure")
			}
			if output.Len() != 0 {
				t.Fatalf("success output after failure: %q", output.String())
			}
		})
	}
}
