import type { Metadata } from "next";
import Link from "next/link";
import { CodeBlock, SetupStep, SiteFooter, SiteHeader } from "../components";

export const metadata: Metadata = {
  title: "Set up your first runner | RunnerYard",
  description: "Go from a private GitHub repository to one green disposable runner without copying a GitHub token.",
};

export default function SetupPage() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <section className="page-intro shell">
          <p className="eyebrow">First-run path</p>
          <h1>From repository to green canary.</h1>
          <p>Start with one trusted private repository. The CLI writes reviewable config, opens GitHub when approval is needed, and keeps every credential out of this website.</p>
          <div className="setup-facts" aria-label="Setup properties">
            <span>No RunnerYard account</span>
            <span>No GitHub token copy-paste</span>
            <span>No workflow switch until you confirm</span>
          </div>
          <p className="state-sequence" aria-label="Setup states">Not started → Browser approval → App installed → Provider ready → Canary queued → Green → Cleanup verified</p>
        </section>

        <div className="setup shell">
          <SetupStep number="01" title="Generate the local scaffold" description="Run this from the target repository. It creates provider config and a standalone canary workflow; it does not deploy or upload anything." receipt="The generated diff contains only .runneryard config and the canary workflow.">
            <CodeBlock>npx runneryard init --github https://github.com/acme/widgets</CodeBlock>
            <p className="note">The default ceiling is intentionally small. Keep it for the canary, then size from observed concurrency using the <a href="https://github.com/gwendall/runneryard/blob/main/docs/configuration.md">configuration reference</a>.</p>
          </SetupStep>

          <SetupStep number="02" title="Create an isolated compute boundary" description="Choose a provider. Keep the trusted controller separate from disposable workers, and keep the worker network away from production." receipt="The controller has durable storage; the worker scope contains zero secrets.">
            <div className="inline-links">
              <Link className="button" href="/providers/fly">Use Fly Machines</Link>
              <Link className="text-link" href="/providers/hetzner">Use Hetzner Cloud</Link>
            </div>
          </SetupStep>

          <SetupStep number="03" title="Connect GitHub in the browser" description="The recommended flow creates a private GitHub App owned by you. GitHub shows its owner, installation target, and exact runner-management permission before approval." receipt="The CLI verifies the installation, stores the key in your selected sink, and prints a non-secret receipt.">
            <CodeBlock>{`npx runneryard auth github create \\
  --github https://github.com/acme/widgets \\
  --controller-app acme-ci-controller`}</CodeBlock>
            <p className="note"><strong>What happens:</strong> the CLI opens a loopback callback, receives the one-time app key, verifies it against the repository you selected, and streams it to Fly over stdin. It is never shown or sent to RunnerYard.</p>
            <p className="note">Already operate a GitHub App? <Link href="/security#bring-your-own">Use bring-your-own mode</Link> with a local PEM file.</p>
          </SetupStep>

          <SetupStep number="04" title="Prove the boundary before deployment" description="Doctor checks the CLIs, app separation, controller credentials, worker secrets, and provider-specific isolation. A failure stops the path." receipt="Every required doctor check says pass; warnings are understood and accepted.">
            <CodeBlock>{`npx runneryard doctor --provider fly \\
  --controller-app acme-ci-controller \\
  --worker-app acme-ci-runners`}</CodeBlock>
            <p className="note">If a check fails, stop and use its safe next action. Diagnostic output reports credential presence and scope, never credential values.</p>
          </SetupStep>

          <SetupStep number="05" title="Deploy and run one canary" description="Initialize the durable budget once, deploy the pinned controller image, then trigger the generated workflow manually." receipt="GitHub is green, status shows the full lifecycle, and provider inventory returns to zero workers.">
            <CodeBlock>{`fly ssh console --app acme-ci-controller \\
  --command '/usr/local/bin/runneryard status'

fly machines list --app acme-ci-runners`}</CodeBlock>
            <p className="note">Use the exact deployment commands on the <Link href="/providers/fly">Fly</Link> or <Link href="/providers/hetzner">Hetzner</Link> page. Never initialize the ledger again during an upgrade.</p>
          </SetupStep>

          <SetupStep number="06" title="Enable the fleet deliberately" description="Keep the hosted runner as the default while you qualify the canary. Enabling changes one repository variable and requires an explicit canary confirmation." receipt="route status reports your qualified label; one low-risk CI job completes there.">
            <CodeBlock>{`npx runneryard route enable \\
  --github https://github.com/acme/widgets \\
  --label acme-linux \\
  --confirm-canary`}</CodeBlock>
            <p className="note">Hosted fallback is a manual repository-variable switch, not an availability detector. Emergency return: <code>npx runneryard route disable --github https://github.com/acme/widgets</code></p>
            <p className="note">For a team or agent-heavy repository, commit the reviewed scaffold and give contributors the <a href="https://github.com/gwendall/runneryard/blob/main/AGENTS.md">repository agent contract</a>. RunnerYard provides compute; causal CI and the protected merge lane remain repository policy.</p>
          </SetupStep>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
