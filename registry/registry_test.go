package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testPassword = "registry-secret"
)

func TestParse(t *testing.T) {
	cases := []struct {
		image, registry, repository, tag, digest, pinned string
	}{
		{"registry.fly.io/app:current", "registry.fly.io", "app", "current", "", "registry.fly.io/app@" + testDigest},
		{"ghcr.io/acme/runner", "ghcr.io", "acme/runner", "latest", "", "ghcr.io/acme/runner@" + testDigest},
		{"ubuntu:24.04", "docker.io", "library/ubuntu", "24.04", "", "ubuntu@" + testDigest},
		{"acme/runner:1", "docker.io", "acme/runner", "1", "", "acme/runner@" + testDigest},
		{"index.docker.io/library/ubuntu", "docker.io", "library/ubuntu", "latest", "", "index.docker.io/library/ubuntu@" + testDigest},
		{"localhost:5000/runner:dev", "localhost:5000", "runner", "dev", "", "localhost:5000/runner@" + testDigest},
		{"Registry.Example/acme/runner:x", "registry.example", "acme/runner", "x", "", "Registry.Example/acme/runner@" + testDigest},
		{"ghcr.io/acme/runner:1@" + testDigest, "ghcr.io", "acme/runner", "1", testDigest, "ghcr.io/acme/runner@" + testDigest},
		{"ghcr.io/acme/runner@" + testDigest, "ghcr.io", "acme/runner", "", testDigest, "ghcr.io/acme/runner@" + testDigest},
	}
	for _, c := range cases {
		ref, err := Parse(c.image)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.image, err)
			continue
		}
		if ref.Registry != c.registry || ref.Repository != c.repository || ref.Tag != c.tag || ref.Digest != c.digest ||
			ref.Pinned() != (c.digest != "") || ref.Pin(testDigest) != c.pinned {
			t.Errorf("Parse(%q) = %+v, pinned %q", c.image, ref, ref.Pin(testDigest))
		}
	}
	for _, image := range []string{
		"", "ghcr.io/", "ghcr.io/acme/runner@sha256:abc", "ghcr.io/acme/runner@md5:" + strings.Repeat("a", 32),
		"ghcr.io/Acme/runner:1", "ghcr.io/acme/runner:-x", "ghcr.io/acme/runner:a b", "bad_host.example/runner:1",
		"registry.example:123456/runner:1", "ghcr.io/acme//runner:1",
	} {
		if ref, err := Parse(image); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", image, ref)
		}
	}
}

// testRegistry serves one repository over TLS; check sees each manifest
// request and answers it.
func testRegistry(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return server, strings.TrimPrefix(server.URL, "https://")
}

func credentialsFor(host, password string) func(string) (string, string, bool) {
	return func(registry string) (string, string, bool) {
		if registry == host {
			return "x", password, true
		}
		return "", "", false
	}
}

func TestDigestReadsTheStoredManifestDigestWithBasicCredentials(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server, host := testRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/v2/acme/runner/manifests/current" || !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if username, password, ok := r.BasicAuth(); !ok || username != "x" || password != testPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", testDigest)
	})
	tag, err := NewTag(host+"/acme/runner:current", &Client{HTTPClient: server.Client(), Credentials: credentialsFor(host, testPassword)})
	if err != nil {
		t.Fatal(err)
	}
	image, err := tag.Resolve(context.Background())
	if err != nil || image != host+"/acme/runner@"+testDigest {
		t.Fatalf("Resolve = %q, %v", image, err)
	}
	if len(seen) != 1 || seen[0] != "HEAD /v2/acme/runner/manifests/current" {
		t.Fatalf("requests = %v, want one HEAD", seen)
	}

	wrong, _ := NewTag(host+"/acme/runner:current", &Client{HTTPClient: server.Client(), Credentials: credentialsFor(host, "wrong-secret")})
	if _, err := wrong.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "refused the credentials") || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("Resolve with a wrong password = %v", err)
	}
	anonymous, _ := NewTag(host+"/acme/runner:current", &Client{HTTPClient: server.Client()})
	if _, err := anonymous.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "requires credentials") {
		t.Fatalf("anonymous Resolve = %v", err)
	}
}

func TestDigestFollowsABearerChallengeToTheRegistry(t *testing.T) {
	server, host := testRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			username, password, ok := r.BasicAuth()
			if !ok || username != "x" || password != testPassword ||
				r.URL.Query().Get("scope") != "repository:acme/runner:pull" || r.URL.Query().Get("service") != "registry" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "pull-token"})
		case r.Header.Get("Authorization") == "Bearer pull-token":
			w.Header().Set("Docker-Content-Digest", testDigest)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+r.Host+`/token",service="registry",scope="repository:acme/runner:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	tag, _ := NewTag(host+"/acme/runner:current", &Client{HTTPClient: server.Client(), Credentials: credentialsFor(host, testPassword)})
	image, err := tag.Resolve(context.Background())
	if err != nil || image != host+"/acme/runner@"+testDigest {
		t.Fatalf("Resolve = %q, %v", image, err)
	}
}

func TestDigestNeverSendsCredentialsToAnotherHost(t *testing.T) {
	var mu sync.Mutex
	tokenAuthorization := []string{}
	tokens := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokenAuthorization = append(tokenAuthorization, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "anonymous-token"})
	}))
	defer tokens.Close()
	server, host := testRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer anonymous-token" {
			w.Header().Set("Docker-Content-Digest", testDigest)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+tokens.URL+`/token",service="registry"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	tag, _ := NewTag(host+"/acme/runner:current", &Client{HTTPClient: server.Client(), Credentials: credentialsFor(host, testPassword)})
	if image, err := tag.Resolve(context.Background()); err != nil || image != host+"/acme/runner@"+testDigest {
		t.Fatalf("Resolve = %q, %v", image, err)
	}
	if len(tokenAuthorization) != 1 || tokenAuthorization[0] != "" {
		t.Fatalf("the token service on another host received %q", tokenAuthorization)
	}

	insecure, insecureHost := testRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="http://tokens.example/token"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	tag, _ = NewTag(insecureHost+"/acme/runner:current", &Client{HTTPClient: insecure.Client(), Credentials: credentialsFor(insecureHost, testPassword)})
	if _, err := tag.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Fatalf("Resolve with a plain-HTTP realm = %v", err)
	}
}

func TestDigestDoesNotFollowARedirect(t *testing.T) {
	var mu sync.Mutex
	hit := false
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hit = true
		mu.Unlock()
		w.Header().Set("Docker-Content-Digest", testDigest)
	}))
	defer elsewhere.Close()
	server, host := testRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	tag, _ := NewTag(host+"/acme/runner:current", &Client{HTTPClient: server.Client(), Credentials: credentialsFor(host, testPassword)})
	if _, err := tag.Resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("Resolve through a redirect = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hit {
		t.Fatal("followed the redirect to another host")
	}
}

func TestDigestHashesTheManifestWhenTheRegistryOmitsItsDigest(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	server, host := testRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write(manifest)
		}
	})
	sum := sha256.Sum256(manifest)
	tag, _ := NewTag(host+"/acme/runner:current", &Client{HTTPClient: server.Client()})
	if image, err := tag.Resolve(context.Background()); err != nil || image != host+"/acme/runner@sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("Resolve = %q, %v", image, err)
	}
}

func TestDigestReportsRegistryFailuresWithoutCredentials(t *testing.T) {
	cases := map[string]struct {
		status int
		digest string
		want   string
	}{
		"missing tag":  {http.StatusNotFound, "", "has no tag current"},
		"forbidden":    {http.StatusForbidden, "", "refused to read"},
		"server error": {http.StatusBadGateway, "", "HTTP 502"},
		"bad digest":   {http.StatusOK, "sha256:not-hex", "unexpected digest"},
	}
	for name, c := range cases {
		server, host := testRegistry(t, func(w http.ResponseWriter, _ *http.Request) {
			if c.digest != "" {
				w.Header().Set("Docker-Content-Digest", c.digest)
			}
			w.WriteHeader(c.status)
		})
		tag, _ := NewTag(host+"/acme/runner:current", &Client{HTTPClient: server.Client(), Credentials: credentialsFor(host, testPassword)})
		_, err := tag.Resolve(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), testPassword) {
			t.Errorf("%s: Resolve = %v, want %q", name, err, c.want)
		}
	}
}

func TestTagFollowsOnlyAnUnpinnedImageAndKnowsItsRepository(t *testing.T) {
	if _, err := NewTag("registry.fly.io/app@"+testDigest, nil); err == nil {
		t.Fatal("NewTag accepted a pinned image")
	}
	if _, err := NewTag("registry.fly.io/App:current", nil); err == nil {
		t.Fatal("NewTag accepted an invalid image")
	}
	tag, err := NewTag("registry.fly.io/app:current", nil)
	if err != nil {
		t.Fatal(err)
	}
	related := map[string]bool{
		"registry.fly.io/app:current":                  true,
		"registry.fly.io/app@" + testDigest:            true,
		"registry.fly.io/app:deployment-1":             true,
		"registry.fly.io/other@" + testDigest:          false,
		"ghcr.io/app@" + testDigest:                    false,
		"registry.fly.io/app@sha256:" + "not-a-digest": false,
		"app@" + testDigest:                            false,
		"registry.fly.io/":                             false,
	}
	for image, want := range related {
		if got := tag.Related(image); got != want {
			t.Errorf("Related(%q) = %v, want %v", image, got, want)
		}
	}
}
