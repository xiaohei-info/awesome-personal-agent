import { Fragment, useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { NavLink, Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { COUNTS_EVENT, GroupContext } from "./page";
import { GROUPS, groupOf, type Feature } from "./registry";
import { useSession } from "./session";
import { toggleTheme } from "./theme";

const MOD = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘" : "Ctrl";

export function Shell({ features: all }: { features: Feature[] }) {
  const features = useMemo(() => all.filter((f) => !f.hidden), [all]); // 稳定引用：下面的 effect 依赖它
  const { api, logout } = useSession();
  const { pathname } = useLocation();
  const [error, setError] = useState<string | null>(null);
  const [counts, setCounts] = useState<Record<string, number>>({});
  const [jumping, setJumping] = useState(false);

  useEffect(() => {
    const refresh = () =>
      features.forEach((f) =>
        f.count?.(api).then(
          (n) => setCounts((c) => ({ ...c, [f.id]: n })),
          () => setCounts((c) => ({ ...c, [f.id]: 0 })), // 取不到就不显示
        ),
      );
    refresh();
    window.addEventListener(COUNTS_EVENT, refresh);
    return () => window.removeEventListener(COUNTS_EVENT, refresh);
  }, [api, features, pathname]);

  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setJumping((o) => !o);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  return (
    <div className="shell">
      <nav aria-label="主导航">
        <div className="brand">
          <span className="mark" aria-hidden="true">决</span>
          <div>
            <b>APA</b>
            <small>事情交给 agent，决定留给你</small>
          </div>
        </div>
        {GROUPS.map((g) => {
          const list = features.filter((f) => groupOf(f) === g);
          return (
            list.length > 0 && (
              <Fragment key={g}>
                <h2>{g}</h2>
                <ul>
                  {list.map((f) => (
                    <li key={f.id}>
                      {/* 有子路径页面的父路径（settings 与 settings/grants）只在完全匹配时高亮 */}
                      <NavLink to={"/" + f.path} end={features.some((o) => o.path.startsWith(f.path + "/"))}>
                        {f.title}
                        {counts[f.id] > 0 && <span className="count">{counts[f.id]}</span>}
                      </NavLink>
                    </li>
                  ))}
                </ul>
              </Fragment>
            )
          );
        })}
        <div className="nav-foot">
          <button className="quiet" aria-label="切换明暗主题" onClick={toggleTheme}>
            明 / 暗
          </button>
          <button className="quiet" onClick={() => logout().catch((e: Error) => setError(e.message))}>
            退出登录
          </button>
        </div>
        {error && <p role="alert">{error}</p>}
      </nav>
      <section>
        <div className="topbar">
          <button className="jump" aria-label="跳转到页面" onClick={() => setJumping(true)}>
            <span>跳转到…</span>
            <span aria-hidden="true">
              <kbd>{MOD}</kbd>
              <kbd>K</kbd>
            </span>
          </button>
        </div>
        <Routes>
          {all.map((f) => (
            <Route key={f.id} path={f.path + "/*"} element={<GroupContext.Provider value={groupOf(f)}>{f.element}</GroupContext.Provider>} />
          ))}
          <Route
            path="*"
            element={
              features.length ? (
                <Navigate to={"/" + features[0].path} replace />
              ) : (
                <main>
                  <p className="empty">暂无可用页面</p>
                </main>
              )
            }
          />
        </Routes>
      </section>
      {jumping && <Palette features={features} onClose={() => setJumping(false)} />}
    </div>
  );
}

/** ⌘K / Ctrl+K 跳转面板：从注册的页面生成，输入过滤、上下键选择、回车跳转、Esc 关闭；关闭后焦点回到打开前的位置。 */
function Palette({ features, onClose }: { features: Feature[]; onClose: () => void }) {
  const navigate = useNavigate();
  const [q, setQ] = useState("");
  const [sel, setSel] = useState(0);
  const back = useRef(document.activeElement as HTMLElement | null);
  useEffect(() => () => back.current?.focus?.(), []);

  const needle = q.trim().toLowerCase();
  const list = features.filter((f) => !needle || [f.title, groupOf(f), f.path].some((s) => s.toLowerCase().includes(needle)));
  const at = Math.min(sel, Math.max(list.length - 1, 0));
  const go = (f: Feature) => {
    onClose();
    navigate("/" + f.path);
  };

  const onKey = (e: KeyboardEvent) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (list.length) setSel((at + (e.key === "ArrowDown" ? 1 : list.length - 1)) % list.length);
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (list[at]) go(list[at]);
    } else if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    } else if (e.key === "Tab") {
      e.preventDefault(); // 面板是模态的，焦点留在输入框
    }
  };

  return (
    <div className="palette-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="palette" role="dialog" aria-modal="true" aria-label="跳转">
        <input
          autoFocus
          role="combobox"
          aria-label="跳转到页面"
          aria-expanded="true"
          aria-controls="palette-list"
          aria-activedescendant={list[at] ? "palette-" + list[at].id : undefined}
          placeholder="跳转到页面…"
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            setSel(0);
          }}
          onKeyDown={onKey}
        />
        <ul id="palette-list" role="listbox" aria-label="页面">
          {list.map((f, n) => (
            <li key={f.id} id={"palette-" + f.id} role="option" aria-selected={n === at} onMouseEnter={() => setSel(n)} onClick={() => go(f)}>
              {f.title} <small>{groupOf(f)}</small>
            </li>
          ))}
        </ul>
        {!list.length && <p className="palette-empty">没有匹配的页面</p>}
        <footer aria-hidden="true">
          <span>
            <kbd>↑</kbd> <kbd>↓</kbd> 选择
          </span>
          <span>
            <kbd>↵</kbd> 打开
          </span>
          <span>
            <kbd>Esc</kbd> 关闭
          </span>
        </footer>
      </div>
    </div>
  );
}
