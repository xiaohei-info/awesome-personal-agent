import { describe, expect, it } from "vitest";
import { parseCron, toCron } from "./schedule";

describe("简报定时的生成与读回", () => {
  it("哪几天 + 几点 → 带时区的 5 段 cron，读回得到同样的选择", () => {
    expect(toCron("1-5", "08:30", "Asia/Shanghai")).toBe("CRON_TZ=Asia/Shanghai 30 8 * * 1-5");
    expect(toCron("*", "07:05", "")).toBe("5 7 * * *");
    expect(parseCron("CRON_TZ=Asia/Shanghai 30 8 * * 1-5")).toEqual({ days: "1-5", time: "08:30", zone: "Asia/Shanghai" });
    expect(parseCron("CRON_TZ=UTC 0 9 * * 0")).toEqual({ days: "0", time: "09:00", zone: "UTC" });
  });

  it("不带时区的写法（开箱默认安排，按 APA 主机时区）读回，zone 为空", () => {
    expect(parseCron("30 8 * * 1-5")).toEqual({ days: "1-5", time: "08:30", zone: "" });
    expect(parseCron("30 17 * * 5")).toEqual({ days: "5", time: "17:30", zone: "" });
  });

  it("更复杂的写法不读回", () => {
    for (const s of ["CRON_TZ=UTC */5 * * * *", "CRON_TZ=UTC 0 8 1 * *", "24h", "CRON_TZ=UTC 0 25 * * *"]) expect(parseCron(s)).toBeNull();
  });
});
