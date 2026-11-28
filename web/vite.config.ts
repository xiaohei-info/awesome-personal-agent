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
    // 样式守线测试按原文读样式文件（style-tokens.check.test.ts）；普通的样式导入仍按默认不处理。
    css: { include: [/\.css\?raw$/] },
    coverage: { provider: "v8", include: ["src/app/**", "src/features/**"], exclude: ["src/**/*.test.*", "src/app/test-setup.ts", "src/app/main.tsx"] },
  },
});
