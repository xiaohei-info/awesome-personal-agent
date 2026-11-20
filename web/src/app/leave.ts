import { useEffect } from "react";

// 未保存的改动：任何一处有，就在关窗口或点站内链接离开前问一句（界面设计 §6.1）。
const dirtyCount = { n: 0 };
/** 这块有未保存的改动时登记一下。 */
export function useDirty(on: boolean) {
  useEffect(() => {
    if (!on) return;
    dirtyCount.n++;
    return () => {
      dirtyCount.n--;
    };
  }, [on]);
}

/** 外壳挂一次：有未保存的改动时，关窗口由浏览器确认，点站内链接先问一句。 */
export function useLeaveGuard(question: string) {
  useEffect(() => {
    const unload = (e: BeforeUnloadEvent) => {
      if (dirtyCount.n > 0) e.preventDefault();
    };
    const click = (e: MouseEvent) => {
      const a = (e.target as HTMLElement | null)?.closest?.("a[href]");
      if (!a || dirtyCount.n === 0 || a.getAttribute("target") === "_blank") return;
      if (!window.confirm(question)) {
        e.preventDefault();
        e.stopPropagation();
      }
    };
    window.addEventListener("beforeunload", unload);
    document.addEventListener("click", click, true);
    return () => {
      window.removeEventListener("beforeunload", unload);
      document.removeEventListener("click", click, true);
    };
  }, [question]);
}

