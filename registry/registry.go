// Package registry reads which digest a container image tag names, so a pool
// can create its hosts from exactly that image and notice when the tag moves.
// It only reads manifests: it never pulls or pushes an image.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	maxManifestBytes   = 4 << 20
	maxTokenBytes      = 64 << 10
	dockerHub          = "docker.io"
	dockerHubAPI       = "registry-1.docker.io"
	// manifestAccept asks for the manifest as the registry stores it, an index
	// included, so the digest is the one an image pinned with it resolves to
	// and does not depend on which platform the registry would pick.
	manifestAccept = "application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, " +
		"application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json"
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	pathPattern   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	hostPattern   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*(?::[0-9]{1,5})?$`)
)

// Reference is a parsed image reference.
type Reference struct {
	// Registry is the registry host, such as registry.fly.io; docker.io for
	// an image named without one.
	Registry string
	// Repository is the path within the registry, library/ included for an
	// official Docker Hub image.
	Repository string
	// Tag is the tag, latest when the reference names neither a tag nor a
	// digest.
	Tag string
	// Digest is the pinned digest, or empty.
	Digest string
	// name is the image name as written, without its tag and digest.
	name string
}

// Parse reads an image reference: [registry/]repository[:tag][@digest].
func Parse(image string) (Reference, error) {
	name, digest, pinned := strings.Cut(image, "@")
	if pinned && !digestPattern.MatchString(digest) {
		return Reference{}, fmt.Errorf("image %q: digest must be sha256 and 64 lowercase hex characters", image)
	}
	tag := ""
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name, tag = name[:colon], name[colon+1:]
		if !tagPattern.MatchString(tag) {
			return Reference{}, fmt.Errorf("image %q: invalid tag", image)
		}
	}
	host, path := dockerHub, name
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		if !hostPattern.MatchString(first) {
			return Reference{}, fmt.Errorf("image %q: invalid registry host", image)
		}
		host, path = strings.ToLower(first), rest
	}
	if host == "index.docker.io" {
		host = dockerHub
	}
	if !pathPattern.MatchString(path) {
		return Reference{}, fmt.Errorf("image %q: invalid repository", image)
	}
	if host == dockerHub && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	if !pinned && tag == "" {
		tag = "latest"
	}
	return Reference{Registry: host, Repository: path, Tag: tag, Digest: digest, name: name}, nil
}

// Pinned reports whether the reference names a digest.
func (r Reference) Pinned() bool { return r.Digest != "" }

// Pin returns the image name, as written, pinned to digest.
func (r Reference) Pin(digest string) string { return r.name + "@" + digest }

// Client reads manifests from registries.
type Client struct {
	HTTPClient *http.Client
	// Credentials returns the user name and password for a registry host, or
	// ok false to read it anonymously. They are sent to that host only.
	Credentials func(registry string) (username, password string, ok bool)
}

// Digest returns the digest of the manifest ref's tag names, as the registry
// stores it: for an image built for several platforms, or with attestations,
// the digest of its index, not of one platform's manifest.
func (c *Client) Digest(ctx context.Context, ref Reference) (string, error) {
	if ref.Tag == "" || ref.Repository == "" || ref.Registry == "" {
		return "", errors.New("registry: the reference names no tag")
	}
	apiHost := ref.Registry
	if apiHost == dockerHub {
		apiHost = dockerHubAPI
	}
	endpoint := "https://" + apiHost + "/v2/" + ref.Repository + "/manifests/" + url.PathEscape(ref.Tag)
	client := c.httpClient()
	authorization := ""
	username, password, credentials := "", "", false
	if c.Credentials != nil {
		username, password, credentials = c.Credentials(ref.Registry)
	}
	if credentials {
		authorization = basic(username, password)
	}
	response, err := c.manifest(ctx, client, http.MethodHead, endpoint, authorization)
	if err != nil {
		return "", err
	}
	if response.StatusCode == http.StatusUnauthorized {
		challenge := response.Header.Get("WWW-Authenticate")
		scheme, params := parseChallenge(challenge)
		if scheme != "bearer" {
			if credentials {
				return "", fmt.Errorf("registry %s refused the credentials for %s (HTTP 401)", ref.Registry, ref.Repository)
			}
			return "", fmt.Errorf("registry %s requires credentials for %s", ref.Registry, ref.Repository)
		}
		token, err := c.token(ctx, client, params, apiHost, ref.Repository, username, password, credentials)
		if err != nil {
			return "", fmt.Errorf("registry %s: %w", ref.Registry, err)
		}
		authorization = "Bearer " + token
		if response, err = c.manifest(ctx, client, http.MethodHead, endpoint, authorization); err != nil {
			return "", err
		}
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("registry %s has no tag %s in %s", ref.Registry, ref.Tag, ref.Repository)
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", fmt.Errorf("registry %s refused to read %s (HTTP %d)", ref.Registry, ref.Repository, response.StatusCode)
	default:
		return "", fmt.Errorf("registry %s answered HTTP %d for %s:%s", ref.Registry, response.StatusCode, ref.Repository, ref.Tag)
	}
	if digest := response.Header.Get("Docker-Content-Digest"); digest != "" {
		if !digestPattern.MatchString(digest) {
			return "", fmt.Errorf("registry %s returned an unexpected digest for %s:%s", ref.Registry, ref.Repository, ref.Tag)
		}
		return digest, nil
	}
	// A registry that leaves the digest out of a HEAD answer: hash the
	// manifest itself, which is what the digest is.
	return c.hashManifest(ctx, client, endpoint, authorization, ref)
}

func (c *Client) httpClient() *http.Client {
	client := &http.Client{Timeout: defaultHTTPTimeout}
	if c.HTTPClient != nil {
		copied := *c.HTTPClient
		client = &copied
	}
	// A redirect could carry the request, and its credentials, to another
	// host: manifests are read where the reference says, or not at all.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func (c *Client) manifest(ctx context.Context, client *http.Client, method, endpoint, authorization string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", manifestAccept)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	_ = response.Body.Close()
	return response, nil
}

func (c *Client) hashManifest(ctx context.Context, client *http.Client, endpoint, authorization string, ref Reference) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", manifestAccept)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry %s answered HTTP %d for %s:%s", ref.Registry, response.StatusCode, ref.Repository, ref.Tag)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxManifestBytes+1))
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}
	if len(body) > maxManifestBytes {
		return "", fmt.Errorf("registry %s returned a manifest larger than %d bytes", ref.Registry, maxManifestBytes)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// token gets a pull token for repository from the realm a Bearer challenge
// names. Credentials go to the realm only when it is the registry itself.
func (c *Client) token(ctx context.Context, client *http.Client, params map[string]string, apiHost, repository, username, password string, credentials bool) (string, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", errors.New("the token realm is not an https URL")
	}
	query := realm.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	query.Set("scope", "repository:"+repository+":pull")
	realm.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if credentials && strings.EqualFold(realm.Host, apiHost) {
		request.Header.Set("Authorization", basic(username, password))
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("get a pull token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the token service answered HTTP %d", response.StatusCode)
	}
	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxTokenBytes)).Decode(&answer); err != nil {
		return "", errors.New("the token service returned no token")
	}
	token := answer.Token
	if token == "" {
		token = answer.AccessToken
	}
	if token == "" || strings.ContainsAny(token, " \r\n") {
		return "", errors.New("the token service returned no token")
	}
	return token, nil
}

func basic(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

// parseChallenge reads a WWW-Authenticate header: its scheme, lowercased, and
// its quoted parameters.
func parseChallenge(header string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	params := map[string]string{}
	for rest = strings.TrimSpace(rest); rest != ""; {
		key, value, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			end := strings.Index(value[1:], `"`)
			if end < 0 {
				break
			}
			params[key] = value[1 : end+1]
			rest = value[end+2:]
		} else {
			params[key], rest, _ = strings.Cut(value, ",")
			params[key] = strings.TrimSpace(params[key])
		}
		rest = strings.TrimLeft(strings.TrimSpace(rest), ",")
		rest = strings.TrimSpace(rest)
	}
	return strings.ToLower(scheme), params
}

// Tag follows one image tag: Resolve pins the image to the digest the tag
// names now.
type Tag struct {
	ref    Reference
	client *Client
}

// NewTag follows image, which must name a tag and no digest.
func NewTag(image string, client *Client) (*Tag, error) {
	ref, err := Parse(image)
	if err != nil {
		return nil, err
	}
	if ref.Pinned() {
		return nil, fmt.Errorf("image %q is pinned to a digest; there is no tag to follow", image)
	}
	if client == nil {
		client = &Client{}
	}
	return &Tag{ref: ref, client: client}, nil
}

// Resolve returns the image pinned to the digest its tag names now, such as
// registry.fly.io/app@sha256:....
func (t *Tag) Resolve(ctx context.Context) (string, error) {
	digest, err := t.client.Digest(ctx, t.ref)
	if err != nil {
		return "", err
	}
	return t.ref.Pin(digest), nil
}

// Related reports whether image names the followed repository: the tag
// itself, another of its tags, or one of its digests.
func (t *Tag) Related(image string) bool {
	ref, err := Parse(image)
	return err == nil && ref.Registry == t.ref.Registry && ref.Repository == t.ref.Repository
}
