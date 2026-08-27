import type { Metadata } from "next";
import { CodeBlock, docsUrl, ProviderHero, SiteFooter, SiteHeader } from "../../components";

export const metadata: Metadata = {
  title: "Hetzner Cloud provider | RunnerYard",
  description: "Run disposable GitHub Actions workers as isolated Hetzner Cloud Docker servers.",
};

export default function HetznerProviderPage() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <ProviderHero
          title="Run on Hetzner Cloud."
          status="Preview"
          description="Keep the controller on any persistent Linux host. RunnerYard creates a clean Hetzner Docker server for each job and deletes it afterward."
        />
        <div className="provider-main shell">
          <section className="provider-section">
            <h2>Before you start</h2>
            <div className="provider-content">
              <ul>
                <li>Create a dedicated Hetzner project that cannot reach production.</li>
                <li>Install <code>hcloud</code> and export a project-scoped <code>HCLOUD_TOKEN</code>.</li>
                <li>Use a private GitHub repository for the first canary.</li>
              </ul>
            </div>
          </section>
          <section className="provider-section">
            <h2>Block inbound traffic</h2>
            <div className="provider-content">
              <p>A Hetzner firewall with no rules drops all inbound traffic and permits outbound traffic. RunnerYard requires its ID and attaches it to every worker.</p>
              <CodeBlock>{`hcloud firewall create --name runneryard-workers
hcloud firewall describe runneryard-workers -o json | jq .id`}</CodeBlock>
            </div>
          </section>
          <section className="provider-section">
            <h2>Generate the setup</h2>
            <div className="provider-content">
              <CodeBlock>{`npx runneryard init \\
  --provider hetzner \\
  --region fsn1 \\
  --github https://github.com/acme/widgets

cp .runneryard/controller.env.example \\
  .runneryard/controller.env`}</CodeBlock>
              <p>Fill the GitHub App values, <code>HCLOUD_TOKEN</code>, and <code>RUNNER_HETZNER_FIREWALL_ID</code>. Put the App key at <code>.runneryard/github-app.pem</code>. The default worker is <code>cpx32</code> on Hetzner&apos;s official <code>docker-ce</code> image.</p>
            </div>
          </section>
          <section className="provider-section">
            <h2>Verify, then deploy</h2>
            <div className="provider-content">
              <CodeBlock>{`npx runneryard doctor --provider hetzner \\
  --firewall-id 123456

docker compose -f .runneryard/hetzner.controller.compose.yml \\
  run --rm controller budget init \\
  --file /var/lib/runneryard/budget.json

docker compose -f .runneryard/hetzner.controller.compose.yml up -d`}</CodeBlock>
              <p>The controller host needs durable Docker storage and outbound access to GitHub and Hetzner. It does not need access to worker VMs.</p>
            </div>
          </section>
          <section className="provider-section">
            <h2>Security boundary</h2>
            <div className="provider-content">
              <ul>
                <li>The Hetzner token and GitHub App key remain on the controller.</li>
                <li>The VM receives only one JIT configuration and a deadline.</li>
                <li>The JIT file is erased before the runner container starts.</li>
                <li>Ownership labels let reconciliation ignore foreign servers.</li>
              </ul>
              <p>Preview means the adapter has unit and integration-seam coverage but still needs a public release canary on a real Hetzner project. Read the <a href={`${docsUrl}/providers/hetzner.md`}>complete Hetzner guide</a> before using it for production CI.</p>
            </div>
          </section>
          <section className="provider-section">
            <h2>Outboarding</h2>
            <div className="provider-content">
              <p>Route workflows back to a hosted label, stop the controller, delete any server carrying the RunnerYard ownership labels, revoke the project token, then remove the controller and worker firewalls.</p>
            </div>
          </section>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
