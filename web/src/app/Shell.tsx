import { useState } from "react";
import { NavLink, Navigate, Route, Routes } from "react-router-dom";
import type { Feature } from "./registry";
import { useSession } from "./session";

export function Shell({ features }: { features: Feature[] }) {
  const { logout } = useSession();
  const [error, setError] = useState<string | null>(null);

  return (
    <div className="shell">
      <nav aria-label="主导航">
        <ul>
          {features.map((f) => (
            <li key={f.id}>
              <NavLink to={"/" + f.path}>{f.title}</NavLink>
            </li>
          ))}
        </ul>
        <button
          onClick={() => logout().catch((e: Error) => setError(e.message))}
        >
          退出登录
        </button>
        {error && <p role="alert">{error}</p>}
      </nav>
      <section>
        <Routes>
          {features.map((f) => (
            <Route key={f.id} path={f.path + "/*"} element={f.element} />
          ))}
          <Route path="*" element={features.length ? <Navigate to={"/" + features[0].path} replace /> : <p>暂无可用页面</p>} />
        </Routes>
      </section>
    </div>
  );
}
