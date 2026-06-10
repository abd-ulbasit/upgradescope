import { useState } from "react";
import { getToken, setToken } from "./api";
import { useHashRoute } from "./hooks";
import { Cluster } from "./views/Cluster";
import { Fleet } from "./views/Fleet";
import { Registry } from "./views/Registry";

// Hash routes: #/ (fleet) · #/cluster/{id}[?target=1.38] · #/registry.
// Hand-rolled on purpose — three routes don't justify a router dependency.
function parseRoute(route: string):
  | { view: "fleet" }
  | { view: "registry" }
  | { view: "cluster"; id: number; target?: string }
  | { view: "notfound" } {
  const [path = "/", search = ""] = route.split("?", 2);
  if (path === "/") return { view: "fleet" };
  if (path === "/registry") return { view: "registry" };
  const m = /^\/cluster\/(\d+)$/.exec(path);
  if (m) {
    const target = new URLSearchParams(search).get("target") ?? undefined;
    return { view: "cluster", id: Number(m[1]), target };
  }
  return { view: "notfound" };
}

export function App() {
  const route = parseRoute(useHashRoute());
  // authEpoch remounts the active view after the token changes so every
  // request re-fires with the new credentials.
  const [authEpoch, setAuthEpoch] = useState(0);

  return (
    <div className="app">
      <header className="topbar">
        <a className="brand" href="#/">
          upgrade<span>scope</span>
        </a>
        <nav aria-label="Primary">
          <a href="#/" aria-current={route.view === "fleet" ? "page" : undefined}>
            Fleet
          </a>
          <a
            href="#/registry"
            aria-current={route.view === "registry" ? "page" : undefined}
          >
            Registry
          </a>
        </nav>
        <TokenSettings onSaved={() => setAuthEpoch((e) => e + 1)} />
      </header>
      <main key={authEpoch}>
        {route.view === "fleet" && <Fleet />}
        {route.view === "registry" && <Registry />}
        {route.view === "cluster" && (
          <Cluster id={route.id} target={route.target} />
        )}
        {route.view === "notfound" && (
          <div className="state">
            <p className="state-title">Page not found</p>
            <a href="#/">Back to the fleet</a>
          </div>
        )}
      </main>
    </div>
  );
}

// TokenSettings: the optional read token (serve --read-token), kept in
// localStorage and sent as a bearer header by the API client.
function TokenSettings({ onSaved }: { onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(getToken);

  const save = () => {
    setToken(value.trim());
    setOpen(false);
    onSaved();
  };

  return (
    <div className="token-settings">
      <button
        type="button"
        className="btn btn-ghost"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        title="API read token"
      >
        {getToken() ? "● token set" : "○ set token"}
      </button>
      {open && (
        <form
          className="token-pop card"
          onSubmit={(e) => {
            e.preventDefault();
            save();
          }}
        >
          <label htmlFor="read-token">Read token</label>
          <input
            id="read-token"
            type="password"
            autoComplete="off"
            placeholder="leave empty for open servers"
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
          <p className="muted">
            Stored in this browser's localStorage only; matches{" "}
            <code>serve --read-token</code>.
          </p>
          <div className="head-row">
            <button type="submit" className="btn">
              Save
            </button>
            <button
              type="button"
              className="btn btn-ghost"
              onClick={() => setOpen(false)}
            >
              Cancel
            </button>
          </div>
        </form>
      )}
    </div>
  );
}
