import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";

const geist = Geist({ variable: "--font-geist", subsets: ["latin"] });
const mono = Geist_Mono({ variable: "--font-geist-mono", subsets: ["latin"] });

export const metadata: Metadata = {
  metadataBase: new URL("https://runneryard.com"),
  title: "RunnerYard | Disposable GitHub Actions runners",
  description: "Run one disposable GitHub Actions worker per job on your cloud. No Kubernetes, hosted control plane, or GitHub token copy-paste.",
  openGraph: {
    title: "RunnerYard | Disposable GitHub Actions runners",
    description: "One clean worker per job, on infrastructure you control.",
    url: "https://runneryard.com",
    siteName: "RunnerYard",
    type: "website",
  },
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body className={`${geist.variable} ${mono.variable}`}>
        {children}
      </body>
    </html>
  );
}
