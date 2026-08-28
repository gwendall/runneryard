import Link from "next/link";
import { CodeBlock, ProviderRow, SiteFooter, SiteHeader } from "./components";

const repositoryUrl = "https://github.com/gwendall/runneryard";

export default function Home() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <section className="hero shell">
          <p className="eyebrow">Self-hosted GitHub Actions runners</p>
          <div className="hero-copy">
            <h1>GitHub Actions runners, in your cloud.</h1>
            <p>RunnerYard starts one clean worker for each job and deletes it afterward. Keep GitHub&apos;s queue and workflow UI without Kubernetes or a hosted runner vendor.</p>
            <CodeBlock>npx runneryard init --github https://github.com/acme/widgets</CodeBlock>
            <div className="actions">
              <Link className="button" href="/setup">Set up a runner</Link>
              <a className="text-link" href={repositoryUrl}>View source</a>
            </div>
            <p className="assurance">Scale from zero · One job per worker · No GitHub token to paste</p>
          </div>
        </section>

        <section className="lifecycle shell" aria-label="Runner lifecycle">
          <div><span>01</span><strong>Job queued</strong><p>GitHub keeps the queue and workflow UI.</p></div>
          <div><span>02</span><strong>Worker launched</strong><p>One clean machine receives a one-job JIT credential.</p></div>
          <div><span>03</span><strong>Worker deleted</strong><p>Completion, timeout, and reconciliation all end in cleanup.</p></div>
        </section>

        <section className="section shell split" aria-labelledby="control-title">
          <div className="section-heading">
            <p className="eyebrow">Control</p>
            <h2 id="control-title">One small control plane.</h2>
          </div>
          <div className="prose">
            <p>A single Go controller watches a GitHub runner scale set. Its compute adapter has three operations: launch, inventory, and destroy.</p>
            <p>Concurrency, maximum lifetime, and a durable rolling usage budget fail closed. A private status receipt shows capacity, assignment latency, provider latency, and remaining budget without opening a monitoring port.</p>
          </div>
        </section>

        <section className="section shell" aria-labelledby="providers-title">
          <div className="section-heading">
            <p className="eyebrow">Compute</p>
            <h2 id="providers-title">Choose the compute account.</h2>
            <p>The controller and credentials stay there.</p>
          </div>
          <div className="provider-list">
            <ProviderRow href="/providers/fly" name="Fly Machines" status="Available" description="Controller and workers run as separate Fly apps." />
            <ProviderRow href="/providers/hetzner" name="Hetzner Cloud" status="Preview" description="Disposable Docker VMs inside a dedicated project." />
            <ProviderRow href="/providers/adapter" name="Another cloud" status="Adapter" description="Implement the narrow provider interface once." />
          </div>
        </section>

        <section className="section shell split" aria-labelledby="security-title">
          <div className="section-heading">
            <p className="eyebrow">Security</p>
            <h2 id="security-title">Jobs get no permanent credential.</h2>
          </div>
          <div className="prose">
            <p>The recommended setup creates a private GitHub App owned by you and installed only where the fleet runs. The browser shows the exact owner and permission; its one-time key moves directly from GitHub to your secret store.</p>
            <p>RunnerYard never receives the key. Workers receive only GitHub&apos;s short-lived JIT configuration and cannot inherit controller or provider secrets.</p>
            <Link className="text-link" href="/security">Read the security model</Link>
          </div>
        </section>

        <section className="closing shell">
          <p className="eyebrow">First run</p>
          <h2>Start with one private canary.</h2>
          <p>Normal CI stays on its current runner until doctor passes, the canary is green, cleanup is verified, and you explicitly enable the fleet.</p>
          <Link className="button" href="/setup">Open the setup guide</Link>
        </section>
      </main>
      <SiteFooter />
    </>
  );
}
