import { Fragment, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { Link, Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { COUNTS_EVENT, SectionContext } from "./page";
import { MOBILE, SETTINGS, SETTINGS_HOME, buildNav, featureAt, settingsGroups, type Entry, type Feature, type NavModel } from "./registry";
import { SettingsHome } from "./SettingsHome";
import { useLang, useT } from "./i18n";
import { BRAND_MARK, common } from "./strings";
import { useSession } from "./session";
import { toggleTheme } from "./theme";

const MOD = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘" : "Ctrl";
// 与 styles.css 的窄屏断点一致：窄屏只渲染手机顶栏，宽屏只渲染侧栏，DOM 里不重复出现同名链接。
const NARROW = "(max-width: 820px)";

function useNarrow() {
  const [narrow, setNarrow] = useState(() => typeof window !== "undefined" && !!window.matchMedia?.(NARROW).matches);
  useEffect(() => {
    const mq = window.matchMedia?.(NARROW);
    if (!mq) return;
    const on = () => setNarrow(mq.matches);
    mq.addEventListener?.("change", on);
    return () => mq.removeEventListener?.("change", on);
  }, []);
  return narrow;
}

export function Shell({ features: all }: { features: Feature[] }) {
  const features = useMemo(() => all.filter((f) => !f.hidden), [all]); // 稳定引用：下面的 effect 依赖它
  const lang = useLang();
  const t = useT(common);
  const nav = useMemo(() => buildNav(features, lang), [features, lang]);
  const entryOf = useMemo(() => new Map(nav.entries.flatMap((e) => e.features.map((f) => [f.id, e] as const))), [nav]);
  const { api, logout } = useSession();
  const { pathname } = useLocation();
  const narrow = useNarrow();
  const [error, setError] = useState<string | null>(null);
  const [counts, setCounts] = useState<Record<string, number>>({});
  const [jumping, setJumping] = useState(false);
  const [more, setMore] = useState(false);

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

  const atHome = pathname.replace(/^\/+|\/+$/g, "") === SETTINGS_HOME;
  const current = atHome ? undefined : featureAt(features, pathname);
  const here = atHome ? nav.settings : current && entryOf.get(current.id);
  const inSettings = !!here && here.id === SETTINGS;
  const sum = (fs: Feature[]) => fs.reduce((n, f) => n + (counts[f.id] ?? 0), 0);
  const signOut = () => logout().catch((e: Error) => setError(e.message));
  const entryLink = (e: Entry, label: string = e.title, onClick?: () => void) => (
    <NavItem to={narrow && e.id === SETTINGS ? SETTINGS_HOME : e.features[0].path} active={e === here} count={sum(e.features)} onClick={onClick}>
      {label}
    </NavItem>
  );
  const bar = nav.entries.filter((e) => (MOBILE as string[]).includes(e.id));
  const routes = <AppRoutes all={all} nav={nav} entryOf={entryOf} noPages={t.noPages} />;

  return (
    <div className="shell">
      <nav aria-label={t.navLabel}>
        <div className="brand">
          <span className="mark" aria-hidden="true">{BRAND_MARK}</span>
          <div>
            <b>APA</b>
            <small>{t.brand}</small>
          </div>
        </div>
        {narrow ? (
          <>
            <ul>
              {bar.map((e) => (
                <li key={e.id}>{entryLink(e, e.short ?? e.title)}</li>
              ))}
            </ul>
            <button
              className={"more" + (here && !bar.includes(here) ? " active" : "")}
              aria-expanded={more}
              aria-haspopup="dialog"
              onClick={() => setMore(true)}
            >
              {t.more}
            </button>
          </>
        ) : (
          <>
            {nav.groups.map((g) => (
              <Fragment key={g.group}>
                <h2>{g.group}</h2>
                <ul>
                  {g.entries.map((e) => (
                    <li key={e.id}>{entryLink(e)}</li>
                  ))}
                </ul>
              </Fragment>
            ))}
            <div className="nav-foot">
              {nav.settings && <ul>{<li>{entryLink(nav.settings)}</li>}</ul>}
              <div>
                <button className="quiet" aria-label={t.themeLabel} onClick={toggleTheme}>
                  {t.theme}
                </button>
                <button className="quiet" onClick={signOut}>
                  {t.logout}
                </button>
              </div>
            </div>
          </>
        )}
        {error && <p role="alert">{error}</p>}
      </nav>
      <section>
        <div className="topbar">
          <button className="jump" aria-label={t.jumpLabel} onClick={() => setJumping(true)}>
            <span>{t.jump}</span>
            <span aria-hidden="true">
              <kbd>{MOD}</kbd>
              <kbd>K</kbd>
            </span>
          </button>
        </div>
        {here && here.features.length > 1 && !inSettings && <Tabs entry={here} current={current} counts={counts} />}
        {inSettings && narrow && !atHome && (
          <Link className="back" to={"/" + SETTINGS_HOME}>
            ‹ {here.title}
          </Link>
        )}
        {inSettings && !narrow ? (
          <div className="settings-layout">
            <SettingsNav entry={here} current={current} />
            <div>{routes}</div>
          </div>
        ) : (
          routes
        )}
      </section>
      {jumping && <Palette nav={nav} onClose={() => setJumping(false)} />}
      {more && (
        <More nav={nav} skip={bar} onClose={() => setMore(false)} link={(e) => entryLink(e, e.title, () => setMore(false))} onLogout={signOut} />
      )}
    </div>
  );
}

function AppRoutes({ all, nav, entryOf, noPages }: { all: Feature[]; nav: NavModel; entryOf: Map<string, Entry>; noPages: string }) {
  return (
        <Routes>
          {nav.settings && <Route path={SETTINGS_HOME + "/*"} element={<SettingsHome entry={nav.settings} />} />}
          {all.map((f) => (
            <Route key={f.id} path={f.path + "/*"} element={<SectionContext.Provider value={entryOf.get(f.id)?.title}>{f.element}</SectionContext.Provider>} />
          ))}
          {all.flatMap((f) => (f.aliases ?? []).map((a) => <Route key={f.id + ":" + a} path={a + "/*"} element={<Alias to={f.path} />} />))}
          <Route
            path="*"
            element={
              nav.entries.length ? (
                <Navigate to={"/" + nav.entries[0].features[0].path} replace />
              ) : (
                <main>
                  <p className="empty">{noPages}</p>
                </main>
              )
            }
          />
        </Routes>
  );
}

function NavItem({ to, active, count, onClick, children }: { to: string; active: boolean; count: number; onClick?: () => void; children: ReactNode }) {
  return (
    <Link to={"/" + to} className={active ? "active" : undefined} aria-current={active ? "page" : undefined} onClick={onClick}>
      {children}
      {count > 0 && <span className="count">{count}</span>}
    </Link>
  );
}

/** 入口内的页签：每个页签就是一个页面的原地址，旧链接直接落到对应页签。 */
function Tabs({ entry, current, counts }: { entry: Entry; current?: Feature; counts: Record<string, number> }) {
  const lang = useLang();
  const t = useT(common);
  const on = useRef<HTMLAnchorElement>(null);
  useEffect(() => {
    on.current?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
  }, [current]);
  return (
    <nav className="tabs" aria-label={t.tabs(entry.title)}>
      {entry.features.map((f) => (
        <Link key={f.id} ref={f === current ? on : undefined} to={"/" + f.path} className={f === current ? "on" : undefined} aria-current={f === current ? "page" : undefined}>
          {f.title[lang]}
          {(counts[f.id] ?? 0) > 0 && <span className="count">{counts[f.id]}</span>}
        </Link>
      ))}
    </nav>
  );
}

/** 设置的第二列（桌面）：按「日常 / agent / 系统」分组列出设置页，永久删除排最后并标红。 */
function SettingsNav({ entry, current }: { entry: Entry; current?: Feature }) {
  const lang = useLang();
  const t = useT(common);
  return (
    <nav className="settings-nav" aria-label={t.tabs(entry.title)}>
      {settingsGroups(entry, lang).map((g) => (
        <Fragment key={g.title}>
          <h2>{g.title}</h2>
          {g.features.map((f) => (
            <Link
              key={f.id}
              to={"/" + f.path}
              className={[f === current ? "on" : "", f.id === "purge" ? "danger" : ""].join(" ").trim() || undefined}
              aria-current={f === current ? "page" : undefined}
            >
              {f.title[lang]}
            </Link>
          ))}
        </Fragment>
      ))}
    </nav>
  );
}

/** 别名地址改写到页面的正式地址，保留查询串与锚点（如 /judgments/spot-check?judgment=…）。 */
function Alias({ to }: { to: string }) {
  const { search, hash } = useLocation();
  return <Navigate to={"/" + to + search + hash} replace />;
}

/** 手机上的"更多"：顶栏放不下的入口、设置、明暗与退出。模态，Esc 或点遮罩关闭，焦点留在抽屉里，关闭后回到打开前的位置。 */
function More({ nav, skip, onClose, link, onLogout }: { nav: NavModel; skip: Entry[]; onClose: () => void; link: (e: Entry) => ReactNode; onLogout: () => void }) {
  const lang = useLang();
  const t = useT(common);
  const box = useRef<HTMLDivElement>(null);
  const back = useRef(document.activeElement as HTMLElement | null);
  useEffect(() => {
    box.current?.querySelector<HTMLElement>("a, button")?.focus();
    const prev = back.current;
    return () => prev?.focus?.();
  }, []);
  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Escape") {
      e.preventDefault();
      onClose();
    } else if (e.key === "Tab") {
      const all = [...(box.current?.querySelectorAll<HTMLElement>("a, button") ?? [])];
      const [first, last] = [all[0], all[all.length - 1]];
      if (e.shiftKey ? document.activeElement === first : document.activeElement === last) {
        e.preventDefault();
        (e.shiftKey ? last : first)?.focus();
      }
    }
  };
  const groups = [...nav.groups.map((g) => ({ group: g.group, entries: g.entries.filter((e) => !skip.includes(e)) })), { group: "", entries: nav.settings ? [nav.settings] : [] }];
  return (
    <div className="sheet-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="sheet" role="dialog" aria-modal="true" aria-label={t.allPages} ref={box} onKeyDown={onKey}>
        {groups.map(
          (g) =>
            g.entries.length > 0 && (
              <Fragment key={g.group}>
                {g.group && <h2>{g.group}</h2>}
                <ul>
                  {g.entries.map((e) => (
                    <li key={e.id}>
                      {link(e)}
                      {e.features.length > 1 && e.features.length <= 3 && <small>{e.features.map((f) => f.title[lang]).join(" · ")}</small>}
                    </li>
                  ))}
                </ul>
              </Fragment>
            ),
        )}
        <div className="sheet-foot">
          <button className="quiet" aria-label={t.themeLabel} onClick={toggleTheme}>
            {t.theme}
          </button>
          <button className="quiet" onClick={onLogout}>
            {t.logout}
          </button>
        </div>
      </div>
    </div>
  );
}

/** ⌘K / Ctrl+K 跳转面板：从注册的页面生成，输入过滤、上下键选择、回车跳转、Esc 关闭；关闭后焦点回到打开前的位置。 */
function Palette({ nav, onClose }: { nav: NavModel; onClose: () => void }) {
  const lang = useLang();
  const t = useT(common);
  const navigate = useNavigate();
  const [q, setQ] = useState("");
  const [sel, setSel] = useState(0);
  const back = useRef(document.activeElement as HTMLElement | null);
  useEffect(() => () => back.current?.focus?.(), []);

  const needle = q.trim().toLowerCase();
  // 按导航顺序列出全部页面；小字是所在入口（与页面同名时不重复）。
  // 两种语言的页面名都能搜到，英文界面下输中文名也能跳转。
  const all = nav.entries.flatMap((e) => e.features.map((f) => ({ f, where: e.title === f.title[lang] ? "" : e.title })));
  const list = all.filter(({ f, where }) => !needle || [f.title.zh, f.title.en, where, f.path].some((s) => s.toLowerCase().includes(needle)));
  const at = Math.min(sel, Math.max(list.length - 1, 0));
  const go = ({ f }: { f: Feature }) => {
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
      <div className="palette" role="dialog" aria-modal="true" aria-label={t.jumpDialog}>
        <input
          autoFocus
          role="combobox"
          aria-label={t.jumpLabel}
          aria-expanded="true"
          aria-controls="palette-list"
          aria-activedescendant={list[at] ? "palette-" + list[at].f.id : undefined}
          placeholder={t.jumpPlaceholder}
          value={q}
          onChange={(e) => {
            setQ(e.target.value);
            setSel(0);
          }}
          onKeyDown={onKey}
        />
        <ul id="palette-list" role="listbox" aria-label={t.jumpList}>
          {list.map((x, n) => (
            <li key={x.f.id} id={"palette-" + x.f.id} role="option" aria-selected={n === at} onMouseEnter={() => setSel(n)} onClick={() => go(x)}>
              {x.f.title[lang]}
              {x.where && <small> {x.where}</small>}
            </li>
          ))}
        </ul>
        {!list.length && <p className="palette-empty">{t.jumpEmpty}</p>}
        <footer aria-hidden="true">
          <span>
            <kbd>↑</kbd> <kbd>↓</kbd> {t.jumpSelect}
          </span>
          <span>
            <kbd>↵</kbd> {t.jumpOpen}
          </span>
          <span>
            <kbd>Esc</kbd> {t.jumpClose}
          </span>
        </footer>
      </div>
    </div>
  );
}
