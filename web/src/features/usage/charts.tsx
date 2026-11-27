import { useEffect } from "react";
import { Bar, BarChart, CartesianGrid, Pie, PieChart, ReferenceDot, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";

// 用量页的两张 Recharts 图。颜色只用主题变量；朱砂红不用于用量。
export interface Series { key: string; label: string; color: string }
export type Bucket = { slot: string; unreported: number } & Record<string, number | string>;

const mix = (v: string, n: number) => `color-mix(in oklch, var(${v}) ${n}%, var(--paper))`;
export const SEGMENT_COLORS = { read: "var(--ok)", fresh: mix("--ink", 32), write: "var(--warn)", output: "var(--ink)" };
export const PART_COLORS = { judge: "var(--ink)", model: "var(--ok)", agent: mix("--ink", 40) };
export const MODEL_COLORS = ["var(--ink)", "var(--ok)", mix("--ink", 55), mix("--ok", 50), mix("--ink", 30), "var(--line-2)"];

/** Recharts 的浮层只用来取当前槽（悬停、点按、键盘都经它）；数值固定显示在图下方，不被手指挡住。 */
function Probe({ active, label, onPick }: { active?: boolean; label?: unknown; onPick: (slot: string) => void }) {
  useEffect(() => {
    if (active && label != null) onPick(String(label));
  }, [active, label, onPick]);
  return null;
}

export function Trend({ data, series, height, tick, format, onPick }: {
  data: Bucket[]; series: Series[]; height: (b: Bucket) => number; tick: (slot: string) => string; format: (n: number) => string; onPick: (slot: string) => void;
}) {
  return (
    <ResponsiveContainer width="100%" height={220} initialDimension={{ width: 640, height: 220 }}>
      <BarChart data={data} margin={{ top: 12, right: 4, bottom: 0, left: 0 }}>
        <CartesianGrid vertical={false} stroke="var(--line)" />
        <XAxis dataKey="slot" tickFormatter={tick} interval="preserveStartEnd" minTickGap={8} stroke="var(--line-2)" tick={{ fill: "var(--ink-3)", fontSize: 11 }} />
        <YAxis width={48} tickFormatter={format} allowDecimals={false} axisLine={false} tickLine={false} tick={{ fill: "var(--ink-3)", fontSize: 11 }} />
        <Tooltip content={<Probe onPick={onPick} />} cursor={{ fill: "var(--sunken)", stroke: "none" }} />
        {series.map((s) => (
          <Bar key={s.key} dataKey={s.key} name={s.label} stackId="a" fill={s.color} maxBarSize={36} isAnimationActive={false} />
        ))}
        {/* 有未回报的槽在柱顶加黄色空心点：实际可能更高。 */}
        {data.filter((b) => b.unreported > 0).map((b) => (
          <ReferenceDot key={b.slot} x={b.slot} y={height(b)} ifOverflow="visible"
            shape={({ cx, cy }: { cx?: number; cy?: number }) => <circle className="usage-unreported" cx={cx} cy={(cy ?? 0) - 6} r={3.5} fill="none" stroke="var(--warn)" strokeWidth={2} />} />
        ))}
      </BarChart>
    </ResponsiveContainer>
  );
}

export function Donut({ data, center }: { data: { name: string; value: number; fill: string }[]; center: string }) {
  return (
    <div className="usage-donut" aria-hidden="true">
      <PieChart width={160} height={160}>
        <Pie data={data} dataKey="value" nameKey="name" innerRadius={52} outerRadius={76} stroke="var(--surface)" isAnimationActive={false} />
      </PieChart>
      <b>{center}</b>
    </div>
  );
}
