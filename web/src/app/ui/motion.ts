// 行离开列表的收起（规格 §5.7「行离开」）：高度、上下内边距与透明度一起收到 0，下面的行顺势补上，约 150ms。
// 用 Web Animations 而不是 CSS 类：要从当前的实际高度收起。系统开了减少动态效果、或环境不支持时直接结束。
const reduced = () => typeof window === "undefined" || !!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;

export function collapse(el: HTMLElement | null): Promise<void> {
  if (!el || reduced() || typeof el.animate !== "function") return Promise.resolve();
  const h = el.getBoundingClientRect().height;
  const a = el.animate(
    [
      { height: h + "px", opacity: 1, overflow: "hidden" },
      { height: "0px", minHeight: "0px", paddingTop: "0px", paddingBottom: "0px", borderBottomWidth: "0px", opacity: 0, overflow: "hidden" },
    ],
    { duration: 150, easing: "cubic-bezier(.23, 1, .32, 1)", fill: "forwards" },
  );
  return a.finished.then(() => undefined, () => undefined);
}
