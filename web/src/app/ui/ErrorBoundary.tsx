import { Component, type ErrorInfo, type ReactNode } from "react";
import { useT } from "../i18n";
import { EmptyState } from "./feedback";
import { strings } from "./strings";

// 错误边界（React 自带的类组件，不加依赖）：子树渲染抛错时只换掉这一块，导航、顶栏和别的区块照常。
// 两层用法：外壳按路由包住内容区（page，resetKey 用地址，换页自动重置）；页面里独立的区块（速览、单个区块）局部包住。
// 它只兜住白屏，不替代修具体缺陷；错误连同组件堆栈写 console.error，界面上只给一句话和「重试」。
export class ErrorBoundary extends Component<{ children: ReactNode; resetKey?: unknown; page?: boolean }, { error: unknown }> {
  state: { error: unknown } = { error: null };

  static getDerivedStateFromError(error: unknown) {
    return { error: error ?? new Error("render failed") };
  }

  componentDidCatch(error: unknown, info: ErrorInfo) {
    console.error("render failed", error, info.componentStack);
  }

  componentDidUpdate(prev: { resetKey?: unknown }) {
    if (this.state.error && prev.resetKey !== this.props.resetKey) this.setState({ error: null });
  }

  render() {
    if (!this.state.error) return this.props.children;
    const fallback = <Fallback page={this.props.page} onRetry={() => this.setState({ error: null })} />;
    return this.props.page ? <main>{fallback}</main> : fallback;
  }
}

function Fallback({ page, onRetry }: { page?: boolean; onRetry: () => void }) {
  const t = useT(strings);
  return (
    <div className="render-failed" role="alert">
      <EmptyState boxed={!page} title={page ? t.failPage : t.failBlock} note={t.failNote}
        action={<button type="button" onClick={onRetry}>{t.retry}</button>} />
    </div>
  );
}
