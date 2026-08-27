const repositoryUrl = "https://github.com/gwendall/runneryard";
const docsUrl = `${repositoryUrl}/blob/main/docs`;
const command = "npx runneryard init --github https://github.com/acme/widgets";

const lifecycle = [
  ["1", "GitHub assigns a job", "The controller listens to one runner scale set. It never needs your repository code."],
  ["2", "A clean worker starts", "Your provider receives a short-lived lease and one GitHub JIT configuration."],
  ["3", "The workflow runs once", "The worker executes the job with the image, CPU, and memory you selected."],
  ["4", "The worker is removed", "Completion, timeout, or reconciliation destroys it. Idle workers default to zero."],
] as const;

const boundaries = [
  ["Credentials stay in control", "GitHub App and provider credentials live only in the trusted controller. A worker gets one JIT configuration."],
  ["Workers stay separate", "Control and worker scopes use separate apps and networks. The doctor command rejects shared Fly apps and worker secrets."],
  ["Compute is bounded first", "Concurrency, maximum lifetime, and a durable rolling runtime budget are checked before a worker can start."],
  ["Cleanup owns only its fleet", "Controller and lease metadata let reconciliation ignore infrastructure created by another controller."],
] as const;

function CodeBlock({ children }: { children: string }) {
  return (
    <pre className="max-w-full overflow-x-auto rounded-[10px] border border-black/15 bg-[#171916] px-4 py-3.5 font-[family-name:var(--font-geist-mono)] text-[13px] leading-6 text-[#f1f3ee] sm:px-5">
      <code><span className="select-none text-[#8ecba3]">$ </span>{children}</code>
    </pre>
  );
}

function Architecture() {
  return (
    <figure className="rounded-[10px] border border-[var(--line-strong)] bg-[var(--surface)] p-5 sm:p-6" aria-labelledby="architecture-caption">
      <figcaption id="architecture-caption" className="mb-5 flex items-center justify-between gap-4 font-[family-name:var(--font-geist-mono)] text-[11px] text-[var(--muted)]">
        <span>One job lifecycle</span>
        <span>Your cloud account</span>
      </figcaption>
      <div className="grid gap-3">
        <div className="rounded-[8px] border border-[var(--line)] bg-white px-4 py-3">
          <p className="text-sm font-semibold">GitHub Actions</p>
          <p className="mt-1 text-xs text-[var(--muted)]">Job assignment and status</p>
        </div>
        <div className="mx-5 h-4 border-l border-[var(--accent)]" aria-hidden="true" />
        <div className="rounded-[8px] border border-[var(--accent)] bg-[var(--accent-soft)] px-4 py-3">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <p className="text-sm font-semibold">RunnerYard controller</p>
            <p className="font-[family-name:var(--font-geist-mono)] text-[10px] text-[var(--accent-ink)]">trusted</p>
          </div>
          <p className="mt-1 text-xs text-[var(--muted)]">GitHub App, provider token, runtime ledger</p>
        </div>
        <div className="mx-5 h-4 border-l border-[var(--accent)]" aria-hidden="true" />
        <div className="rounded-[8px] border border-dashed border-[var(--line-strong)] bg-white px-4 py-3">
          <div className="flex flex-wrap items-baseline justify-between gap-2">
            <p className="text-sm font-semibold">Ephemeral worker</p>
            <p className="font-[family-name:var(--font-geist-mono)] text-[10px] text-[var(--muted)]">untrusted</p>
          </div>
          <p className="mt-1 text-xs text-[var(--muted)]">One JIT config, one job, then destroyed</p>
        </div>
      </div>
      <div className="mt-5 grid grid-cols-2 gap-3 border-t border-[var(--line)] pt-4 font-[family-name:var(--font-geist-mono)] text-[11px] text-[var(--muted)]">
        <span>idle workers: 0</span>
        <span className="text-right">worker secrets: 0</span>
      </div>
    </figure>
  );
}

export default function Home() {
  return (
    <>
      <a href="#content" className="skip-link">Skip to content</a>
      <header className="mx-auto flex h-16 max-w-6xl items-center justify-between border-b border-[var(--line)] px-5 sm:px-8">
        <a href="#top" className="font-[family-name:var(--font-geist-mono)] text-sm font-semibold tracking-[-0.03em]">RunnerYard</a>
        <nav aria-label="Primary navigation" className="flex items-center gap-5 text-sm text-[var(--muted)] sm:gap-7">
          <a className="hidden hover:text-[var(--ink)] sm:block" href="#how">How it works</a>
          <a className="hidden hover:text-[var(--ink)] sm:block" href="#security">Security</a>
          <a className="rounded-[8px] border border-[var(--line-strong)] bg-white px-3 py-1.5 font-medium text-[var(--ink)] hover:border-black/35" href={repositoryUrl}>GitHub</a>
        </nav>
      </header>

      <main id="content">
        <section id="top" className="mx-auto grid max-w-6xl grid-cols-1 gap-12 px-5 pb-20 pt-16 sm:px-8 sm:pb-24 sm:pt-20 lg:grid-cols-[1.08fr_0.92fr] lg:items-center lg:gap-20">
          <div className="min-w-0">
            <p className="font-[family-name:var(--font-geist-mono)] text-xs font-medium text-[var(--accent-ink)]">Open source / MIT</p>
            <h1 className="mt-5 max-w-3xl text-[clamp(2.2rem,4.5vw,3.25rem)] font-semibold leading-[1.04] tracking-[-0.05em]">Ephemeral GitHub runners on your cloud.</h1>
            <p className="mt-6 max-w-xl text-lg leading-8 text-[var(--muted)]">Start one isolated worker per job, enforce a runtime budget, and remove the worker when the job ends.</p>
            <div className="mt-8 flex flex-wrap items-center gap-4">
              <a href="#quickstart" className="rounded-[8px] bg-[var(--ink)] px-4 py-2.5 text-sm font-semibold text-white hover:bg-black active:translate-y-px">Read the quickstart</a>
              <a href={`${docsUrl}/security.md`} className="text-sm font-medium underline decoration-black/25 underline-offset-4 hover:decoration-black">Review the security model</a>
            </div>
            <p className="mt-6 max-w-lg text-sm leading-6 text-[var(--muted)]">No Kubernetes. No hosted control plane. The first adapter runs on Fly Machines; the compute interface stays provider-neutral.</p>
          </div>
          <Architecture />
        </section>

        <section id="how" className="border-y border-[var(--line)] bg-white/45">
          <div className="mx-auto max-w-6xl px-5 py-16 sm:px-8 sm:py-20">
            <h2 className="text-2xl font-semibold tracking-[-0.035em] sm:text-3xl">One machine for one job.</h2>
            <p className="mt-3 max-w-2xl text-base leading-7 text-[var(--muted)]">GitHub keeps the workflow experience. Your account supplies disposable compute.</p>
            <div className="mt-10 grid border-t border-[var(--line)] md:grid-cols-2 lg:grid-cols-4">
              {lifecycle.map(([number, title, body]) => (
                <article key={number} className="border-b border-[var(--line)] py-6 md:px-6 md:odd:border-r lg:border-b-0 lg:border-r lg:first:pl-0 lg:last:border-r-0 lg:last:pr-0">
                  <span className="font-[family-name:var(--font-geist-mono)] text-xs text-[var(--accent-ink)]">{number.padStart(2, "0")}</span>
                  <h3 className="mt-4 text-sm font-semibold">{title}</h3>
                  <p className="mt-2 text-sm leading-6 text-[var(--muted)]">{body}</p>
                </article>
              ))}
            </div>
          </div>
        </section>

        <section id="security" className="mx-auto max-w-6xl px-5 py-16 sm:px-8 sm:py-24">
          <div className="grid grid-cols-1 gap-10 lg:grid-cols-[0.72fr_1.28fr] lg:gap-20">
            <div>
              <p className="font-[family-name:var(--font-geist-mono)] text-xs font-medium text-[var(--accent-ink)]">Security model</p>
              <h2 className="mt-4 text-2xl font-semibold tracking-[-0.035em] sm:text-3xl">Job code stays outside the control plane.</h2>
              <p className="mt-4 text-base leading-7 text-[var(--muted)]">Workers are treated as hostile and disposable. Permanent credentials never enter the job environment.</p>
              <a className="mt-5 inline-block text-sm font-medium underline decoration-black/25 underline-offset-4 hover:decoration-black" href={`${docsUrl}/security.md`}>Read every trust boundary</a>
            </div>
            <div className="border-t border-[var(--line-strong)]">
              {boundaries.map(([title, body]) => (
                <article key={title} className="grid gap-2 border-b border-[var(--line)] py-5 sm:grid-cols-[0.72fr_1.28fr] sm:gap-8">
                  <h3 className="text-sm font-semibold">{title}</h3>
                  <p className="text-sm leading-6 text-[var(--muted)]">{body}</p>
                </article>
              ))}
            </div>
          </div>
        </section>

        <section className="border-y border-[var(--line)] bg-[var(--surface)]">
          <div className="mx-auto grid max-w-6xl grid-cols-1 gap-10 px-5 py-16 sm:px-8 sm:py-20 lg:grid-cols-[0.8fr_1.2fr] lg:items-center lg:gap-20">
            <div>
              <h2 className="text-2xl font-semibold tracking-[-0.035em] sm:text-3xl">Compute stops at your limit.</h2>
              <p className="mt-4 max-w-xl text-base leading-7 text-[var(--muted)]">The controller reserves worst-case runtime before launch. Missing or corrupt budget state queues jobs instead of resetting the ceiling.</p>
            </div>
            <pre className="overflow-x-auto rounded-[10px] border border-[var(--line-strong)] bg-white p-5 font-[family-name:var(--font-geist-mono)] text-xs leading-7 text-[var(--ink)]"><code>{`MIN_RUNNERS=0\nMAX_RUNNERS=4\nRUNNER_MAX_LIFETIME=2h\nRUNNER_USAGE_BUDGET=166h40m`}</code></pre>
          </div>
        </section>

        <section id="quickstart" className="mx-auto max-w-6xl px-5 py-16 sm:px-8 sm:py-24">
          <div className="max-w-2xl">
            <h2 className="text-2xl font-semibold tracking-[-0.035em] sm:text-3xl">Scaffold it in one command.</h2>
            <p className="mt-4 text-base leading-7 text-[var(--muted)]">The initializer writes local configuration and a canary workflow. It does not create infrastructure or upload credentials.</p>
          </div>
          <div className="mt-8 max-w-3xl"><CodeBlock>{command}</CodeBlock></div>
          <ol className="mt-8 grid gap-6 border-t border-[var(--line)] pt-6 sm:grid-cols-3">
            <li><span className="font-[family-name:var(--font-geist-mono)] text-xs text-[var(--accent-ink)]">01</span><p className="mt-2 text-sm font-semibold">Review the files</p><p className="mt-1 text-sm leading-6 text-[var(--muted)]">Nothing is deployed by init.</p></li>
            <li><span className="font-[family-name:var(--font-geist-mono)] text-xs text-[var(--accent-ink)]">02</span><p className="mt-2 text-sm font-semibold">Create isolated apps</p><p className="mt-1 text-sm leading-6 text-[var(--muted)]">Then run doctor before deploy.</p></li>
            <li><span className="font-[family-name:var(--font-geist-mono)] text-xs text-[var(--accent-ink)]">03</span><p className="mt-2 text-sm font-semibold">Run one canary</p><p className="mt-1 text-sm leading-6 text-[var(--muted)]">Verify the worker disappears.</p></li>
          </ol>
          <div className="mt-8 flex flex-wrap gap-4">
            <a className="rounded-[8px] bg-[var(--ink)] px-4 py-2.5 text-sm font-semibold text-white hover:bg-black active:translate-y-px" href={`${docsUrl}/quickstart.md`}>Open the full quickstart</a>
            <a className="rounded-[8px] border border-[var(--line-strong)] bg-white px-4 py-2.5 text-sm font-semibold hover:border-black/35 active:translate-y-px" href={`${docsUrl}/providers/fly.md`}>Deploy on Fly</a>
          </div>
        </section>

        <section className="border-t border-[var(--line)] bg-white/45">
          <div className="mx-auto grid max-w-6xl grid-cols-1 gap-8 px-5 py-12 sm:px-8 lg:grid-cols-[0.8fr_1.2fr] lg:items-start lg:gap-20">
            <div>
              <h2 className="text-xl font-semibold tracking-[-0.03em]">Bring the provider you trust.</h2>
              <p className="mt-3 text-sm leading-6 text-[var(--muted)]">The core depends on launch, inventory, and destroy. Provider details stay in adapters.</p>
            </div>
            <div className="grid gap-5 border-t border-[var(--line-strong)] pt-5 sm:grid-cols-2">
              <div><p className="text-sm font-semibold">Fly Machines</p><p className="mt-2 text-sm leading-6 text-[var(--muted)]">Bundled now. Separate apps, networks, and zero-idle workers.</p></div>
              <div><p className="text-sm font-semibold">Other clouds</p><p className="mt-2 text-sm leading-6 text-[var(--muted)]">Implement the small compute contract without changing orchestration.</p><a className="mt-3 inline-block text-sm font-medium underline decoration-black/25 underline-offset-4 hover:decoration-black" href={`${docsUrl}/adapter-contract.md`}>Read the adapter contract</a></div>
            </div>
          </div>
        </section>
      </main>

      <footer className="border-t border-[var(--line)]">
        <div className="mx-auto flex max-w-6xl flex-col gap-4 px-5 py-7 text-sm text-[var(--muted)] sm:flex-row sm:items-center sm:justify-between sm:px-8">
          <p>RunnerYard / MIT licensed</p>
          <div className="flex gap-5"><a className="hover:text-[var(--ink)]" href={repositoryUrl}>Source</a><a className="hover:text-[var(--ink)]" href={`${docsUrl}/security.md`}>Security</a><a className="hover:text-[var(--ink)]" href={`${docsUrl}/operations.md`}>Operations</a></div>
        </div>
      </footer>
    </>
  );
}
