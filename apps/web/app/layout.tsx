import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "./globals.css";

const geist = Geist({ variable: "--font-geist", subsets: ["latin"] });
const mono = Geist_Mono({ variable: "--font-geist-mono", subsets: ["latin"] });

export const metadata: Metadata = {
  metadataBase: new URL("https://runneryard.com"),
  title: "RunnerYard | GitHub Actions runners on your cloud",
  description: "Run isolated, ephemeral GitHub Actions workers on infrastructure you control, with explicit security and runtime limits.",
};

export default function RootLayout({ children }: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body className={`${geist.variable} ${mono.variable} font-[family-name:var(--font-geist)] antialiased`}>
        {children}
      </body>
    </html>
  );
}
