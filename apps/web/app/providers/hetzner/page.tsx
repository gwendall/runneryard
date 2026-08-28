import type { Metadata } from "next";
import { CodeBlock, docsUrl, ProviderHero, SiteFooter, SiteHeader } from "../../components";

export const metadata: Metadata = {
  title: "Hetzner Cloud provider | RunnerYard",
  description: "Run disposable GitHub Actions workers as isolated Hetzner Cloud Docker servers from a persistent controller host.",
};

export default function HetznerProviderPage() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <ProviderHero title="Run on Hetzner Cloud." status="Preview" description="Put the controller on any persistent Docker host. It starts a clean Hetzner server for each job, verifies its deny-inbound firewall, and deletes it afterward." />
        <div className="provider-main shell">
          <section className="provider-section">
            <h2>01 · Isolate</h2>
            <div className="provider-content">
              <p>Create a dedicated Hetzner project that cannot reach production. Install <code>hcloud</code>, authenticate to that project, and create a worker firewall with no inbound rules.</p>
              <CodeBlock>{`hcloud firewall create --name runneryard-workers
hcloud firewall describe runneryard-workers -o json | jq .id`}</CodeBlock>
              <p className="step-receipt"><span>Ready when</span>The project is dedicated to CI and the worker firewall contains zero inbound rules.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>02 · Scaffold</h2>
            <div className="provider-content">
              <CodeBlock>{`npx runneryard init \\
  --provider hetzner \\
  --region fsn1 \\
  --github https://github.com/acme/widgets`}</CodeBlock>
              <p>The generated Compose setup keeps durable state on the controller host and mounts the GitHub App key read-only.</p>
              <p className="step-receipt"><span>Ready when</span>The generated diff contains Compose, an environment template, ignores for secret files, and the canary.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>03 · Connect GitHub</h2>
            <div className="provider-content">
              <p>Run this on the controller checkout. GitHub opens in your browser; the verified one-time App key is written directly to mode-0600 ignored files. No browser secret is copied into a terminal.</p>
              <CodeBlock>{`npx runneryard auth github create \\
  --github https://github.com/acme/widgets \\
  --sink file`}</CodeBlock>
              <p className="step-receipt"><span>Ready when</span><code>.runneryard/github-app.pem</code> and <code>github-app.env</code> are private and ignored.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>04 · Configure compute</h2>
            <div className="provider-content">
              <p>Create <code>.runneryard/controller.env</code> on the controller host. Add only a read/write token from the dedicated Hetzner project and the firewall ID. Keep the file mode <code>0600</code>.</p>
              <CodeBlock>{`cp .runneryard/controller.env.example .runneryard/controller.env
chmod 600 .runneryard/controller.env

# Set on the controller host only:
# HCLOUD_TOKEN, RUNNER_HETZNER_FIREWALL_ID`}</CodeBlock>
              <p className="step-receipt"><span>Ready when</span>The controller files are private; no token or PEM is committed.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>05 · Verify and start</h2>
            <div className="provider-content">
              <CodeBlock>{`npx runneryard doctor --provider hetzner --firewall-id 123456

docker compose -f .runneryard/hetzner.controller.compose.yml \\
  run --rm controller budget init \\
  --file /var/lib/runneryard/budget.json

docker compose -f .runneryard/hetzner.controller.compose.yml up -d`}</CodeBlock>
              <p className="step-receipt"><span>Ready when</span>Doctor passes and the private status receipt reports a healthy controller.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>06 · Canary and route</h2>
            <div className="provider-content">
              <CodeBlock>{`gh workflow run runneryard-canary.yml --repo acme/widgets

docker compose -f .runneryard/hetzner.controller.compose.yml \\
  exec controller /usr/local/bin/runneryard status
hcloud server list --selector runneryard-managed-by=true

npx runneryard route enable \\
  --github https://github.com/acme/widgets \\
  --label acme-linux --confirm-canary`}</CodeBlock>
              <p>Preview means the adapter has seam coverage but still needs a public release canary in a real Hetzner project. Use only trusted, low-risk workloads until then. <a href={`${docsUrl}/configuration.md`}>Size capacity and budget</a>, then read the <a href={`${docsUrl}/providers/hetzner.md`}>complete Hetzner guide</a>.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>Upgrade</h2>
            <div className="provider-content">
              <p>Disable the route and stop the old controller. Pin the same release in both generated files: <code>RUNNER_IMAGE</code> in <code>controller.env</code> for workers and <code>image</code> in Compose for the controller. Set the canary&apos;s expected version to match. Preserve durable state, start one controller, then prove the controller with status and the worker with the canary.</p>
            </div>
          </section>

          <section className="provider-section">
            <h2>Outboard</h2>
            <div className="provider-content">
              <p>Disable the route, stop the controller, delete owned servers, and revoke the project token. Delete a dedicated GitHub App; for a shared BYO App, remove only the local credential and this repository&apos;s access. Then delete the dedicated project.</p>
              <CodeBlock>npx runneryard route disable --github https://github.com/acme/widgets</CodeBlock>
            </div>
          </section>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
