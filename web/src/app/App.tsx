import { Route, Routes } from "react-router-dom";
import { features as registered, type Feature } from "./registry";
import { Login } from "./Login";
import { Shell } from "./Shell";
import { SessionProvider, useSession } from "./session";
import { useT } from "./i18n";
import { common } from "./strings";

function Gate({ features }: { features: Feature[] }) {
  const { status, error, retry } = useSession();
  const t = useT(common);
  if (status === "loading") return <p className="boot">{t.loading}</p>;
  if (status === "error")
    return (
      <main>
        <p role="alert">{t.sessionError(error ?? "")}</p>
        <button onClick={retry}>{t.retry}</button>
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
