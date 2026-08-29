import type { Metadata } from "next";
import { CodeBlock, docsUrl, ProviderHero, SiteFooter, SiteHeader } from "../../components";

export const metadata: Metadata = {
  title: "Fly Machines provider | RunnerYard",
  description: "Deploy disposable GitHub Actions workers on Fly Machines with a dedicated GitHub App and separate control and worker apps.",
};

export default function FlyProviderPage() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <ProviderHero title="Run on Fly Machines." status="Available" description="The shortest production-piloted path. A small controller app starts one Machine per GitHub job in a separate, secret-free worker app." />
        <div className="provider-main shell">
          <section className="provider-section">
            <h2>01 · Scaffold</h2>
            <div className="provider-content">
              <p>Install and authenticate <code>flyctl</code> and <code>gh</code>. Start in a trusted private repository.</p>
              <CodeBlock>{`npx runneryard init \\
  --provider fly \\
  --github https://github.com/acme/widgets \\
  --max-runners 2`}</CodeBlock>
              <p className="step-receipt"><span>Ready when</span>The generated diff contains one controller config and one isolated canary workflow.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>02 · Isolate</h2>
            <div className="provider-content">
              <p>Control and worker apps must be separate because every Machine in a Fly app inherits that app&apos;s secrets. Workers stay on a separate private network with zero idle capacity.</p>
              <CodeBlock>{`fly apps create acme-ci-runners --network runneryard-workers
fly apps create acme-ci-controller --network runneryard-control
fly volumes create runneryard_state \\
  --app acme-ci-controller --region cdg --size 1 --yes`}</CodeBlock>
              <p className="step-receipt"><span>Ready when</span>Both apps exist, the controller volume is attached, and the worker app has no secrets.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>03 · Connect GitHub</h2>
            <div className="provider-content">
              <p>The CLI opens GitHub to create your private, owner-controlled App. Its one-time key goes straight to the controller app over stdin; you never copy or see it.</p>
              <CodeBlock>{`npx runneryard auth github create \\
  --github https://github.com/acme/widgets \\
  --controller-app acme-ci-controller`}</CodeBlock>
              <p className="step-receipt"><span>Ready when</span>The CLI reports a verified installation for the exact repository.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>04 · Add compute access</h2>
            <div className="provider-content">
              <p>Create a deploy token scoped only to the worker app and store it only on the controller. This is a Fly credential, not a GitHub token. Keep reviewed policy such as <code>MAX_RUNNERS</code> in TOML, never in secrets or pull-request-controlled input.</p>
              <CodeBlock>{`fly tokens create deploy --app acme-ci-runners
fly secrets set --app acme-ci-controller \\
  FLY_API_TOKEN='<worker-app deploy token>'`}</CodeBlock>
              <p className="step-receipt"><span>Ready when</span>The worker app has zero secrets; the controller has GitHub App credentials and one worker-app token.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>05 · Budget and deploy</h2>
            <div className="provider-content">
              <p>Initialize the fail-closed budget ledger exactly once, then run doctor before the controller starts. The generated worker uses two sustained performance CPUs and 8 GB of RAM; shared CPUs remain an explicit option for short, bursty jobs.</p>
              <CodeBlock>{`fly machine run ghcr.io/gwendall/runneryard:0.3.14 \\
  --entrypoint "/usr/local/bin/controller-entrypoint" \\
  --env RUNNER_BUDGET_FILE=/var/lib/runneryard/budget.json \\
  --app acme-ci-controller --region cdg \\
  --volume runneryard_state:/var/lib/runneryard --rm \\
  -- budget init --file /var/lib/runneryard/budget.json

npx runneryard doctor --provider fly \\
  --controller-app acme-ci-controller \\
  --worker-app acme-ci-runners

fly deploy --app acme-ci-controller \\
  --config .runneryard/fly.controller.toml \\
  --image ghcr.io/gwendall/runneryard:0.3.14 --ha=false`}</CodeBlock>
              <p>Each shared vCPU has a 6.25% sustained baseline plus burst capacity. Performance workers cost more per second, but avoid the post-burst slowdown that can turn tests into timeouts and reruns. Measure cost per successful job and keep the operator-owned ceiling low until a representative canary proves the workload. Older scaffolds that omit <code>RUNNER_CPU_KIND</code> remain shared on upgrade; set both shape variables to opt in. <a href={`${docsUrl}/configuration.md#fly-worker-shape`}>Review worker sizing</a>.</p>
              <p className="step-receipt"><span>Ready when</span>Doctor passes and the controller status is healthy with zero desired workers.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>06 · Canary and route</h2>
            <div className="provider-content">
              <p>Trigger the generated workflow. A valid canary proves the pinned Node toolcache, Fly-compatible <code>fuse-overlayfs</code>, and a real BuildKit build and container run. It also has a complete controller lifecycle and leaves no worker behind. Only then route a low-risk job.</p>
              <CodeBlock>{`gh workflow run runneryard-canary.yml --repo acme/widgets

fly ssh console --app acme-ci-controller \\
  --command '/usr/local/bin/runneryard status'
fly machines list --app acme-ci-runners

npx runneryard route enable \\
  --github https://github.com/acme/widgets \\
  --label acme-linux --confirm-canary`}</CodeBlock>
              <p>Keep <code>ubuntu-latest</code> available as the explicit emergency route. <a href={`${docsUrl}/configuration.md`}>Size capacity and budget</a>, then read the <a href={`${docsUrl}/providers/fly.md`}>complete Fly guide</a> for rotation and recovery details.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>Outboard</h2>
            <div className="provider-content">
              <p>Disable the route, finish active jobs, stop the controller, and verify zero workers. Revoke the worker-app token and delete the two apps and volume. Revoke and delete a dedicated GitHub App. For a shared BYO App, remove this controller&apos;s local credential; change installation scope only after confirming no other consumer needs it.</p>
              <CodeBlock>npx runneryard route disable --github https://github.com/acme/widgets</CodeBlock>
            </div>
          </section>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
