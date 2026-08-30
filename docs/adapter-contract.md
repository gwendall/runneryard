# Compute adapter contract

`runneryard` separates GitHub orchestration from compute through the
`provider.Compute` interface. The controller must not import a provider SDK or
know about regions, instance types, cloud-init, OCI runtimes, or cloud identity.

## Interface

```go
type Compute interface {
    Launch(context.Context, Lease) (Worker, error)
    Inventory(context.Context) ([]Worker, error)
    Destroy(context.Context, string) error
}
```

- `Launch` creates exactly one worker for a one-job lease. The only credential
  in a lease is GitHub's short-lived JIT configuration.
- `Inventory` returns every live worker owned by this controller and no foreign
  worker. Ownership must round-trip through provider metadata or tags.
- `Destroy` force-deletes a worker and succeeds when it is already absent.

An adapter keeps image, shape, region, bootstrap, provider credentials, retry,
and state translation inside its implementation. It must never put provider or
controller credentials into worker environment variables, metadata, user data,
logs, or disk.

An adapter retries throttling, provider-side errors, and transport failures
itself, with backoff and request pacing (`provider/retry`), and returns a
`provider.TransientError` once its attempts are exhausted so the core keeps
the listener alive. A create may only be repeated after the adapter has
confirmed through inventory that the lease has no worker. Authorization,
validation, and identity failures must never be reported as transient.

## Required capabilities

An adapter is accepted only if it can prove:

1. unique ownership and lease metadata;
2. isolated workers that accept one JIT job;
3. Docker support when the selected runner profile requires it;
4. no automatic restart after the runner exits;
5. idempotent forced deletion;
6. inventory after controller restart;
7. a controller-enforced maximum lifetime;
8. cleanup after an ambiguous or partial create.

Native provider auto-destroy is defense in depth. The controller remains the
authority for completion, expiry, orphan adoption, and deletion.

## Adapter layout

Put first-party adapters under `providers/<name>`. Their public constructor
accepts provider-specific configuration and returns `provider.Compute`. Tests
must use the provider's HTTP seam or emulator and verify credential isolation,
ownership filtering, idempotent deletion, and partial-create cleanup.

Bundled examples and likely strategies:

- OCI compute (Fly, Railway-like platforms): launch the immutable runner image
  and pass the JIT configuration through the provider's secret injection.
- VM compute (the bundled Hetzner preview, DigitalOcean, EC2, GCE): boot a
  pre-baked image or minimal cloud-init payload, attach a deny-inbound firewall,
  and tag it with lease ownership.
- Restricted container PaaS: reject the profile when privileged Docker and
  reliable forced deletion cannot be guaranteed.
