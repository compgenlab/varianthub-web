import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  build: {
    // Built into the Go module's embed directory, so the binary serves the SPA.
    outDir: "embed/dist",
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    // `npm run dev` proxies the API so the browser sees one origin and CORS
    // never enters the picture during development.
    proxy: {
      "/api": { target: process.env.VITE_DEV_API ?? "http://localhost:18080", changeOrigin: true },
      "/healthz": { target: process.env.VITE_DEV_API ?? "http://localhost:18080", changeOrigin: true },
      "/version": { target: process.env.VITE_DEV_API ?? "http://localhost:18080", changeOrigin: true },
    },
  },
});
