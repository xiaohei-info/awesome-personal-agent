import { lazy, Suspense } from "react";
import { useT } from "../../app/i18n";
import type { Feature } from "../../app/registry";
import { strings } from "./strings";

// 图表库只在打开用量页时下载。分块取不到（断网，或升级后旧页面还开着）时说明并给刷新按钮。
const Page = lazy(() => import("./UsagePage").then((m) => ({ default: m.UsagePage }), () => ({ default: LoadFailed })));

function LoadFailed() {
  const t = useT(strings);
  return (
    <main>
      <p role="alert">{t.loadFailed}</p>
      <button onClick={() => window.location.reload()}>{t.reload}</button>
    </main>
  );
}

function Loading() {
  const t = useT(strings);
  return <main><p role="status">{t.loading}</p></main>;
}

export const feature: Feature = {
  id: "usage", title: { zh: "用量", en: "Usage" }, path: "usage", section: "usage",
  element: <Suspense fallback={<Loading />}><Page /></Suspense>,
};
