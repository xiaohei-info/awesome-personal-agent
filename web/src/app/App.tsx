import { Route, Routes } from "react-router-dom";
import { features as registered, type Feature } from "./registry";
import { Login } from "./Login";
import { Shell } from "./Shell";
import { SessionProvider, useSession } from "./session";

function Gate({ features }: { features: Feature[] }) {
  const { status, error, retry } = useSession();
  if (status === "loading") return <p className="boot">加载中…</p>;
  if (status === "error")
    return (
      <main>
        <p role="alert">无法确认登录状态：{error}</p>
        <button onClick={retry}>重试</button>
      </main>
    );
  return status === "authed" ? <Shell features={features} /> : <Login />;
}

export function App({ features = registered, fetchImpl }: { features?: Feature[]; fetchImpl?: typeof fetch }) {
  return (
    <Routes>
      {features.filter((f) => f.public).map((f) => (
        <Route key={f.id} path={f.path + "/*"} element={f.element} />
      ))}
      <Route
        path="*"
        element={
          <SessionProvider fetchImpl={fetchImpl}>
            <Gate features={features.filter((f) => !f.public)} />
          </SessionProvider>
        }
      />
    </Routes>
  );
}
