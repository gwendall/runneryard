import Link from "next/link";
import { Command } from "./command";

export const repositoryUrl = "https://github.com/gwendall/runneryard";
export const docsUrl = `${repositoryUrl}/blob/main/docs`;

export function SiteHeader() {
  return (
    <>
      <a href="#content" className="skip-link">Skip to content</a>
      <header className="site-header shell">
        <Link className="wordmark" href="/">RunnerYard</Link>
        <nav aria-label="Primary navigation">
          <Link href="/setup">Setup</Link>
          <Link href="/providers">Providers</Link>
          <Link href="/security">Security</Link>
          <a href={repositoryUrl}>GitHub</a>
        </nav>
      </header>
    </>
  );
}

export function SiteFooter() {
  return (
    <footer className="site-footer shell">
      <span>Open source. Runs in your account.</span>
      <nav aria-label="Footer navigation">
        <Link href="/setup">Setup</Link>
        <Link href="/security">Security</Link>
        <a href={repositoryUrl}>Source</a>
      </nav>
    </footer>
  );
}

export function CodeBlock({ children }: { children: string }) {
  return <Command>{children}</Command>;
}

export function ProviderRow({
  href,
  name,
  status,
  description,
}: {
  href: string;
  name: string;
  status: string;
  description: string;
}) {
  return (
    <Link className="provider-row" href={href}>
      <span className="provider-name">{name}</span>
      <span className="status">{status}</span>
      <span className="provider-description">{description}</span>
      <span className="arrow" aria-hidden="true">→</span>
    </Link>
  );
}

export function ProviderHero({
  title,
  status,
  description,
}: {
  title: string;
  status: string;
  description: string;
}) {
  return (
    <section className="provider-hero shell">
      <Link className="back-link" href="/providers">← Providers</Link>
      <div className="provider-title-row">
        <h1>{title}</h1>
        <span className="status">{status}</span>
      </div>
      <p>{description}</p>
    </section>
  );
}

export function SetupStep({
  number,
  title,
  description,
  receipt,
  children,
}: {
  number: string;
  title: string;
  description: string;
  receipt: string;
  children?: React.ReactNode;
}) {
  return (
    <section className="setup-step">
      <div className="step-number" aria-hidden="true">{number}</div>
      <div className="step-content">
        <h2>{title}</h2>
        <p>{description}</p>
        {children}
        <p className="step-receipt"><span>Ready when</span>{receipt}</p>
      </div>
    </section>
  );
}
