import { useEffect, useRef, useState } from "react";
import { clearScope, getToken, isTokenRemembered, setToken } from "./api";
import { useHashRoute, useScope } from "./hooks";
import { SET_TOKEN_LABEL } from "./ui";
import { Cluster } from "./views/Cluster";
import { Fleet } from "./views/Fleet";
import { Registry } from "./views/Registry";
import { Teams } from "./views/Teams";

// Hash routes: #/ (fleet)[?q=&filter=&sort=&target=] · #/teams[?target=1.38] ·
// #/cluster/{id}[?target=1.38][&team=&category=&severity=&q=&finding=] ·
// #/registry. The views read their own query (useRouteQuery); the route
// here only picks the view and its identity.
// Hand-rolled on purpose — four routes don't justify a router dependency.
type Route =
  | { view: "fleet" }
  | { view: "registry" }
  | { view: "teams"; target?: string }
  | { view: "cluster"; id: number; target?: string }
  | { view: "notfound" };

function parseRoute(route: string): Route {
  const i = route.indexOf("?");
  const path = i < 0 ? route : route.slice(0, i);
  const search = i < 0 ? "" : route.slice(i + 1);
  const params = new URLSearchParams(search);
  const target = params.get("target") ?? undefined;
  if (path === "/") return { view: "fleet" };
  if (path === "/registry") return { view: "registry" };
  if (path === "/teams") return { view: "teams", target };
  const m = /^\/cluster\/(\d+)$/.exec(path);
  if (m) {
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
          <a href="#/teams" aria-current={route.view === "teams" ? "page" : undefined}>
            Teams
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
      <ScopeNote />
      <main key={authEpoch}>
        {route.view === "fleet" && <Fleet />}
        {route.view === "teams" && <Teams key={route.target ?? ""} target={route.target} />}
        {route.view === "registry" && <Registry />}
        {route.view === "cluster" && (
          // Keyed by cluster and target: the findings filters are in the
          // URL, and picking another target drops them, so a category
          // picked for one target can never hide every finding of the next.
          <Cluster key={`${route.id}:${route.target ?? ""}`} id={route.id} target={route.target} />
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

// ScopeNote says that the view is filtered when the server answered for a
// team-scoped credential: other teams' clusters, findings and scores are
// left out by the server, not hidden here.
function ScopeNote() {
  const scope = useScope();
  if (scope === null) return null;
  return (
    <p className="scope-note" role="note">
      Showing{" "}
      {scope.length === 1 ? "team " : "teams "}
      {scope.map((t, i) => (
        <span key={t}>
          {i > 0 && ", "}
          <strong>{t}</strong>
        </span>
      ))}{" "}
      only: this view does not include other teams' clusters, findings or
      scores.
    </p>
  );
}

// TokenSettings: the optional read token (serve --read-token), sent as a
// bearer header by the API client. Kept in sessionStorage (this tab, until
// it closes) unless "remember" puts it in localStorage.
function TokenSettings({ onSaved }: { onSaved: () => void }) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(getToken);
  const [remember, setRemember] = useState(isTokenRemembered);
  const toggle = useRef<HTMLButtonElement>(null);
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (open) input.current?.focus();
  }, [open]);

  const close = () => {
    setOpen(false);
    toggle.current?.focus();
  };

  const save = () => {
    setToken(value.trim(), { remember });
    clearScope();
    close();
    onSaved();
  };

  const hasToken = getToken() !== "";
  return (
    <div className="token-settings">
      <button
        ref={toggle}
        type="button"
        className="btn btn-ghost"
        aria-expanded={open}
        aria-controls="token-pop"
        onClick={() => setOpen((o) => !o)}
        title="API read token"
      >
        <span aria-hidden="true">{hasToken ? "● " : "○ "}</span>
        {hasToken ? "token set" : SET_TOKEN_LABEL}
      </button>
      {open && (
        <form
          id="token-pop"
          className="token-pop card"
          onSubmit={(e) => {
            e.preventDefault();
            save();
          }}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              e.preventDefault();
              close();
            }
          }}
        >
          <label htmlFor="read-token">Read token</label>
          <input
            ref={input}
            id="read-token"
            type="password"
            autoComplete="off"
            placeholder="leave empty for open servers"
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
          <label className="check">
            <input
              type="checkbox"
              checked={remember}
              onChange={(e) => setRemember(e.target.checked)}
            />
            Remember on this device
          </label>
          <p className="muted">
            <code>serve --read-token</code>, or a token from{" "}
            <code>tokens create --read</code>, which may read only some
            teams. Kept for this tab only
            (sessionStorage) unless remembered, which stores it in this
            browser's localStorage.
          </p>
          <div className="head-row">
            <button type="submit" className="btn">
              Save
            </button>
            <button type="button" className="btn btn-ghost" onClick={close}>
              Cancel
            </button>
          </div>
        </form>
      )}
    </div>
  );
}
