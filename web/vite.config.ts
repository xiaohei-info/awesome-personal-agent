/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  // 开发时把 API 转给本机 apa serve；生产由同一个二进制同源提供。
  server: { proxy: { "/v1": "http://127.0.0.1:7878" } },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/app/test-setup.ts"],
    coverage: { provider: "v8", include: ["src/app/**", "src/features/**"], exclude: ["src/**/*.test.*", "src/app/test-setup.ts", "src/app/main.tsx"] },
  },
});
