import Link from "next/link";
import { CodeBlock, ProviderRow, SiteFooter, SiteHeader } from "./components";

const repositoryUrl = "https://github.com/gwendall/runneryard";

export default function Home() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <section className="hero shell">
          <div className="hero-copy">
            <h1>GitHub Actions runners on your cloud.</h1>
            <p>One clean worker per job. No Kubernetes, hosted control plane, or permanent worker credentials.</p>
            <CodeBlock>npx runneryard init --github https://github.com/acme/widgets</CodeBlock>
            <div className="actions">
              <Link className="button" href="/providers">Choose a provider</Link>
              <a className="text-link" href={repositoryUrl}>View source</a>
            </div>
          </div>
        </section>

        <section className="section shell" aria-labelledby="flow-title">
          <div className="section-heading">
            <h2 id="flow-title">GitHub stays the control surface.</h2>
            <p>RunnerYard changes where jobs execute, not how your team writes or reviews workflows.</p>
          </div>
          <div className="flow" aria-label="Runner lifecycle">
            <span>GitHub queue</span><b aria-hidden="true">→</b>
            <span>RunnerYard</span><b aria-hidden="true">→</b>
            <span>One clean worker</span><b aria-hidden="true">→</b>
            <span>Deleted</span>
          </div>
        </section>

        <section className="section shell" aria-labelledby="providers-title">
          <div className="section-heading">
            <h2 id="providers-title">Run it where you already trust.</h2>
            <p>The controller and credentials stay in your account. Pick the compute adapter that fits your infrastructure.</p>
          </div>
          <div className="provider-list">
            <ProviderRow
              href="/providers/fly"
              name="Fly Machines"
              status="Available"
              description="The fastest path. Controller and disposable workers run as separate Fly apps."
            />
            <ProviderRow
              href="/providers/hetzner"
              name="Hetzner Cloud"
              status="Preview"
              description="A Docker VM starts for each job inside a dedicated Hetzner project."
            />
            <ProviderRow
              href="/providers/adapter"
              name="Another provider"
              status="Adapter"
              description="Implement launch, inventory, and destroy without changing the controller."
            />
          </div>
        </section>

        <section className="section shell split" id="security" aria-labelledby="security-title">
          <div className="section-heading">
            <h2 id="security-title">The job never gets cloud credentials.</h2>
            <p>Workers are treated as hostile from the moment they start.</p>
          </div>
          <ul className="plain-list">
            <li><strong>Controller</strong><span>Holds the GitHub App key, provider token, and durable budget ledger.</span></li>
            <li><strong>Worker</strong><span>Receives one short-lived JIT configuration and a hard deadline.</span></li>
            <li><strong>Network</strong><span>Has no route to production and no inbound access by default.</span></li>
            <li><strong>Cost</strong><span>Reserves worst-case runtime before launch and stops at your ceiling.</span></li>
          </ul>
        </section>

        <section className="section shell split" id="install" aria-labelledby="install-title">
          <div className="section-heading">
            <h2 id="install-title">Prove one runner before moving CI.</h2>
            <p>The initializer writes local files only. Nothing is deployed and no credential is uploaded.</p>
          </div>
          <div>
            <CodeBlock>{`npx runneryard init --github https://github.com/acme/widgets
npx runneryard doctor --provider fly \\
  --controller-app acme-ci-controller \\
  --worker-app acme-ci-runners`}</CodeBlock>
            <p className="receipt">A successful canary leaves three receipts: a green GitHub job, a complete controller lifecycle, and zero workers left behind.</p>
            <Link className="text-link" href="/providers">Open a provider guide</Link>
          </div>
        </section>
      </main>
      <SiteFooter />
    </>
  );
}
