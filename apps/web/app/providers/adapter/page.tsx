import type { Metadata } from "next";
import { CodeBlock, docsUrl, ProviderHero, SiteFooter, SiteHeader } from "../../components";

export const metadata: Metadata = {
  title: "Build a provider adapter | RunnerYard",
  description: "Connect RunnerYard to another compute provider through its three-operation Go interface.",
};

export default function AdapterProviderPage() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <ProviderHero
          title="Bring another provider."
          status="Three methods"
          description="The controller owns GitHub, concurrency, budgets, and cleanup policy. An adapter only translates one disposable worker into provider infrastructure."
        />
        <div className="provider-main shell">
          <section className="provider-section">
            <h2>The interface</h2>
            <div className="provider-content">
              <CodeBlock>{`type Compute interface {
  Launch(context.Context, Lease) (Worker, error)
  Inventory(context.Context) ([]Worker, error)
  Destroy(context.Context, string) error
}`}</CodeBlock>
            </div>
          </section>
          <section className="provider-section">
            <h2>What the adapter owns</h2>
            <div className="provider-content">
              <p>Image selection, machine shape, region, bootstrap, provider authentication, ownership metadata, and provider state translation.</p>
            </div>
          </section>
          <section className="provider-section">
            <h2>Acceptance bar</h2>
            <div className="provider-content">
              <ul>
                <li>One JIT configuration starts exactly one isolated worker.</li>
                <li>Inventory returns owned workers and excludes everything else.</li>
                <li>Destroy is forced, idempotent, and safe after a partial launch.</li>
                <li>No provider or controller credential enters job code.</li>
                <li>Controller restart can adopt and later remove every worker.</li>
              </ul>
              <p>Read the <a href={`${docsUrl}/adapter-contract.md`}>full adapter contract</a> and use the Fly and Hetzner tests as executable examples.</p>
            </div>
          </section>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
