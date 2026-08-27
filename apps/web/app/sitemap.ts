import type { MetadataRoute } from "next";

export default function sitemap(): MetadataRoute.Sitemap {
  const paths = ["", "/providers", "/providers/fly", "/providers/hetzner", "/providers/adapter"];
  return paths.map((path) => ({ url: `https://runneryard.com${path}` }));
}
