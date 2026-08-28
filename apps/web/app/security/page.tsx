import type { Metadata } from "next";
import { CodeBlock, docsUrl, SiteFooter, SiteHeader } from "../components";

export const metadata: Metadata = {
  title: "Security model | RunnerYard",
  description: "RunnerYard trust boundaries, GitHub App ownership, credential handling, cost controls, and safe removal.",
};

export default function SecurityPage() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <section className="page-intro shell">
          <p className="eyebrow">Security model</p>
          <h1>Treat every CI job as hostile.</h1>
          <p>The controller is trusted. Workers are temporary and untrusted. The design is meant to keep a compromised job away from permanent credentials, other installations, and production networks.</p>
        </section>

        <section className="boundary shell" aria-label="Credential boundary">
          <div><span>Trusted</span><strong>Controller</strong><p>GitHub App key<br />Provider credential<br />Durable budget</p></div>
          <b aria-hidden="true">one-job JIT →</b>
          <div><span>Untrusted</span><strong>Worker</strong><p>No permanent secret<br />No inbound access<br />Hard deadline</p></div>
          <b aria-hidden="true">→</b>
          <div><span>Final state</span><strong>Deleted</strong><p>Normal completion<br />Timeout<br />Reconciliation</p></div>
        </section>

        <div className="security-sections shell">
          <section className="security-section">
            <div><p className="eyebrow">Recommended</p><h2>Your own dedicated GitHub App</h2></div>
            <div className="prose">
              <p>RunnerYard creates a private App owned by your user or organization and installs it only on the selected repositories. GitHub places repository runner scale-set management behind Administration write; organization fleets use the narrower Self-hosted runners write permission. No webhook or OAuth permission is requested.</p>
              <p>The manifest flow is protected by random state and a local callback. GitHub returns a one-time private key to the CLI; it is verified and written directly to your controller secret sink without being printed.</p>
            </div>
          </section>

          <section className="security-section" id="bring-your-own">
            <div><p className="eyebrow">Alternative</p><h2>Bring your own App</h2></div>
            <div className="prose">
              <p>Use an existing private App when your organization already manages key issuance and rotation. RunnerYard verifies the app identity, installation owner, repository access, and required permission before storing anything.</p>
              <CodeBlock>{`chmod 600 ./existing-app.pem
npx runneryard auth github import \\
  --github https://github.com/acme/widgets \\
  --client-id Iv1.example \\
  --installation-id 123456 \\
  --private-key-file ./existing-app.pem \\
  --controller-app acme-ci-controller`}</CodeBlock>
            </div>
          </section>

          <section className="security-section">
            <div><p className="eyebrow">Not offered</p><h2>A shared hosted RunnerYard App</h2></div>
            <div className="prose">
              <p>One central App is safe only when a hosted broker retains its private key, authenticates every controller, authorizes each installation, and mints short-lived tokens. Shipping the central private key to customer controllers would let any one of them sign as the App.</p>
              <p>RunnerYard does not operate that broker today. The default stays fully self-hosted and operator-owned. A hosted control plane can be added later as a separate, explicit trust model—not hidden inside the CLI.</p>
            </div>
          </section>

          <section className="security-section">
            <div><p className="eyebrow">Worker isolation</p><h2>No credential inheritance</h2></div>
            <div className="prose">
              <p>Controller and workers must live in separate secret scopes. Every worker receives only a one-job JIT configuration, runs with no restart policy, and has a maximum lifetime. Do not expose self-hosted runners to untrusted public fork pull requests.</p>
              <p>Provider credentials should be limited to a dedicated worker app or project. The worker network must have no route to production, controller internals, or cloud metadata services.</p>
            </div>
          </section>

          <section className="security-section">
            <div><p className="eyebrow">Cost containment</p><h2>Admission fails closed</h2></div>
            <div className="prose">
              <p>Before launch, the controller durably reserves the job&apos;s worst-case runtime. New work stays queued when concurrency or rolling usage ceilings are reached. Missing or unreadable budget state stops the controller.</p>
              <p>Provider-side spending alerts remain useful because network, image storage, the controller, and provider price changes sit outside the runtime ceiling.</p>
            </div>
          </section>

          <section className="security-section" id="outboarding">
            <div><p className="eyebrow">Outboarding</p><h2>Leave without RunnerYard</h2></div>
            <div className="prose">
              <ol>
                <li>Disable the fleet route and confirm jobs target <code>ubuntu-latest</code>.</li>
                <li>Stop the controller after active jobs finish; verify zero workers.</li>
                <li>Revoke the provider token.</li>
                <li>Dedicated App: revoke its keys, uninstall it, and delete it.</li>
                <li>Shared BYO App: remove only the local credential and this repository&apos;s access. Keep shared keys, installations, and the App until every consumer rotates away.</li>
                <li>Delete provider resources, generated config, local credential files, and the repository variable.</li>
              </ol>
              <p><a className="text-link" href={`${docsUrl}/security.md`}>Read the complete threat model</a></p>
            </div>
          </section>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
