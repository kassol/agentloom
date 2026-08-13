import path from "path";
import react from "@vitejs/plugin-react";
import { playwright } from "@vitest/browser-playwright";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  optimizeDeps: {
    include: [
      "@base-ui/react/button",
      "@base-ui/react/dialog",
      "@base-ui/react/popover",
      "@streamdown/cjk",
      "@streamdown/code",
      "@streamdown/math",
      "@streamdown/mermaid",
      "class-variance-authority",
      "clsx",
      "i18next",
      "lucide-react",
      "react-i18next",
      "rehype-harden",
      "rehype-sanitize",
      "streamdown",
      "tailwind-merge",
    ],
  },
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  test: {
    browser: {
      enabled: true,
      headless: true,
      instances: [{ browser: "chromium" }],
      provider: playwright(),
    },
    include: ["src/**/*.browser.test.{ts,tsx}"],
  },
});
