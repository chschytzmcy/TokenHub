import type { NextConfig } from "next";

// The Next.js dev server rejects requests whose Origin is not listed in
// allowedDevOrigins. A browser that reaches the console over the LAN address
// then loads the page but never hydrates it, which shows up as a blank screen
// with no console error. Extra origins come from the environment so a
// deployment can allow its own address without editing this file.
const extraDevOrigins = (process.env.TOKENHUB_DEV_ALLOWED_ORIGINS ?? "")
  .split(",")
  .map((value) => value.trim())
  .filter(Boolean);

const nextConfig: NextConfig = {
  reactStrictMode: true,
  output: "standalone",
  // Keep isolated UI runs independent of unrelated lockfiles above the checkout.
  turbopack: process.env.TOKENHUB_UI_RUN === "1" ? { root: process.cwd() } : undefined,
  allowedDevOrigins: ["127.0.0.1", "localhost", ...extraDevOrigins],
  distDir: process.env.TOKENHUB_NEXT_DIST_DIR || ".next",
  async headers() {
    return [
      {
        source: "/(.*)",
        headers: [
          { key: "Permissions-Policy", value: "camera=(), geolocation=(), microphone=()" },
          { key: "Referrer-Policy", value: "no-referrer" },
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "X-Frame-Options", value: "DENY" },
        ],
      },
    ];
  },
};

export default nextConfig;
