import type { Metadata } from "next";
import { ProviderRow, SiteFooter, SiteHeader } from "../components";

export const metadata: Metadata = {
  title: "Providers | RunnerYard",
  description: "Deploy disposable GitHub Actions runners on Fly Machines, Hetzner Cloud, or your own compute adapter.",
};

export default function ProvidersPage() {
  return (
    <>
      <SiteHeader />
      <main id="content">
        <section className="provider-hero shell">
          <div className="provider-title-row"><h1>Choose where jobs run.</h1></div>
          <p>Every adapter uses the same controller, security boundary, cost ceiling, and one-job worker lifecycle.</p>
        </section>
        <section className="provider-main shell">
          <div className="provider-list">
            <ProviderRow
              href="/providers/fly"
              name="Fly Machines"
              status="Available"
              description="Smallest setup surface and the current live production pilot."
            />
            <ProviderRow
              href="/providers/hetzner"
              name="Hetzner Cloud"
              status="Preview"
              description="Disposable Docker VMs with a project token and deny-inbound firewall."
            />
            <ProviderRow
              href="/providers/adapter"
              name="Your provider"
              status="Adapter"
              description="Add a cloud with a narrow, provider-neutral Go interface."
            />
          </div>
        </section>
      </main>
      <SiteFooter />
    </>
  );
}
