import { useEffect, useRef, useState } from "react";
import { getToken, isTokenRemembered, setToken } from "./api";
import { useHashRoute } from "./hooks";
import { SET_TOKEN_LABEL } from "./ui";
import { Cluster } from "./views/Cluster";
import { Fleet } from "./views/Fleet";
import { Registry } from "./views/Registry";
import { Teams } from "./views/Teams";

// Hash routes: #/ (fleet) · #/teams[?target=1.38] ·
// #/cluster/{id}[?target=1.38][&team=payments] · #/registry.
// Hand-rolled on purpose — four routes don't justify a router dependency.
type Route =
  | { view: "fleet" }
  | { view: "registry" }
  | { view: "teams"; target?: string }
  | { view: "cluster"; id: number; target?: string; team?: string }
  | { view: "notfound" };

function parseRoute(route: string): Route {
  const [path = "/", search = ""] = route.split("?", 2);
  const params = new URLSearchParams(search);
  const target = params.get("target") ?? undefined;
  if (path === "/") return { view: "fleet" };
  if (path === "/registry") return { view: "registry" };
  if (path === "/teams") return { view: "teams", target };
  const m = /^\/cluster\/(\d+)$/.exec(path);
  if (m) {
    const team = params.get("team") ?? undefined;
    return { view: "cluster", id: Number(m[1]), target, team };
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
      <main key={authEpoch}>
        {route.view === "fleet" && <Fleet />}
        {route.view === "teams" && <Teams key={route.target ?? ""} target={route.target} />}
        {route.view === "registry" && <Registry />}
        {route.view === "cluster" && (
          // Keyed by the whole route: a new target or team filter starts
          // from fresh filter state, so a category picked for one target
          // can never hide every finding of the next.
          <Cluster
            key={`${route.id}:${route.target ?? ""}:${route.team ?? ""}`}
            id={route.id}
            target={route.target}
            team={route.team}
          />
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
            Matches <code>serve --read-token</code>. Kept for this tab only
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
