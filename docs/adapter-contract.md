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
  in a lease is GitHub's short-lived JIT configuration. The lease also carries a
  non-secret deadline and idle timeout that the adapter must deliver to the
  worker as `RUNNERYARD_DEADLINE` (RFC 3339) and `RUNNERYARD_IDLE_TIMEOUT`
  (seconds, `0` disables); `RUNNERYARD_DIAG_HOLD` (seconds) is optional.
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

A provider-enforced account, project, region, or machine ceiling is different
from both classes. Return `provider.CapacityError` with a stable, non-secret
reason code. The adapter must not retry that rejected create: the controller
records the effective fleet capacity, keeps existing lifecycles serviceable,
and performs one probe after its controller-level backoff. Never put a raw API
body, tenant identifier, or credential in the reason code.

Every other permanent response to a create (a validation error, a conflict,
a missing image) is handled by the core as a bounded launch rejection: it
proves through inventory that the lease has no worker, releases the JIT
registration and the reservation, and probes again on the capacity schedule
instead of stopping. Return it as a `retry.StatusError` carrying the provider
name and HTTP status so the reported code (`fly_status_422`) stays stable
and non-secret. Only `401` and `403` are treated as identity failures that
stop the controller.

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
