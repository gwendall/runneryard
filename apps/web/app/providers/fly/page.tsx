import type { Metadata } from "next";
import { CodeBlock, docsUrl, ProviderHero, SiteFooter, SiteHeader } from "../../components";

export const metadata: Metadata = {
  title: "Fly Machines provider | RunnerYard",
  description: "Run disposable GitHub Actions workers on Fly Machines with separate control and worker apps.",
};

export default function FlyProviderPage() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <ProviderHero
          title="Run on Fly Machines."
          status="Available"
          description="The shortest route to a working fleet. Fly runs the controller and starts one Machine for each GitHub job."
        />
        <div className="provider-main shell">
          <section className="provider-section">
            <h2>Start here</h2>
            <div className="provider-content">
              <p>Use a private repository for the first canary. Install the Fly CLI, authenticate it, then generate the repository files.</p>
              <CodeBlock>{`npx runneryard init \\
  --provider fly \\
  --github https://github.com/acme/widgets`}</CodeBlock>
            </div>
          </section>
          <section className="provider-section">
            <h2>Create the boundary</h2>
            <div className="provider-content">
              <p>Control and worker apps must be separate. Workers inherit no app secrets and idle capacity stays at zero.</p>
              <CodeBlock>{`fly apps create acme-ci-runners --network runneryard-workers
fly apps create acme-ci-controller --network runneryard-control
fly volumes create runneryard_state \\
  --app acme-ci-controller --region cdg --size 1 --yes`}</CodeBlock>
            </div>
          </section>
          <section className="provider-section">
            <h2>Verify, then deploy</h2>
            <div className="provider-content">
              <CodeBlock>{`npx runneryard doctor --provider fly \\
  --controller-app acme-ci-controller \\
  --worker-app acme-ci-runners

fly deploy --app acme-ci-controller \\
  --config .runneryard/fly.controller.toml \\
  --image ghcr.io/gwendall/runneryard:0.2.1 --ha=false`}</CodeBlock>
              <p>Initialize the durable budget ledger once before the controller starts. The <a href={`${docsUrl}/providers/fly.md`}>complete Fly guide</a> includes credentials, the exact ledger command, and rollback.</p>
              <p>Keep limits such as <code>MAX_RUNNERS</code> in the generated config, not in Fly secrets. The doctor rejects secret values that would silently override those limits.</p>
            </div>
          </section>
          <section className="provider-section">
            <h2>What to verify</h2>
            <div className="provider-content">
              <ul>
                <li>The canary job is green on the generated scale-set label.</li>
                <li>Controller logs show created, started, completed, and destroyed.</li>
                <li><code>fly machines list --app acme-ci-runners</code> returns to zero.</li>
              </ul>
            </div>
          </section>
          <section className="provider-section">
            <h2>Outboarding</h2>
            <div className="provider-content">
              <p>Route workflows back to <code>ubuntu-latest</code>, stop the controller, verify zero workers, revoke both credentials, then delete the two apps and volume.</p>
            </div>
          </section>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
