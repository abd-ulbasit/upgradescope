// @vitest-environment happy-dom
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { App } from "./App";
import { TOKEN_KEY } from "./api";
import { jsonResponse, mockApi, navigate } from "./test-utils";

function tokenButton(): HTMLElement {
  return screen.getByRole("button", { name: /token/ });
}

describe("token settings", () => {
  it("closes on Escape and returns focus to its toggle", async () => {
    mockApi({ "api/v1/fleet": { targets: [], clusters: [] } });
    render(<App />);
    fireEvent.click(tokenButton());
    const input = screen.getByLabelText("Read token");
    expect(document.activeElement).toBe(input);

    fireEvent.keyDown(input, { key: "Escape" });
    expect(screen.queryByLabelText("Read token")).toBeNull();
    expect(document.activeElement).toBe(tokenButton());
    expect(tokenButton().getAttribute("aria-expanded")).toBe("false");
  });

  it("keeps the token for this tab unless asked to remember it", async () => {
    mockApi({ "api/v1/fleet": { targets: [], clusters: [] } });
    render(<App />);

    fireEvent.click(tokenButton());
    fireEvent.change(screen.getByLabelText("Read token"), { target: { value: "tab" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(sessionStorage.getItem(TOKEN_KEY)).toBe("tab");
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();

    fireEvent.click(tokenButton());
    fireEvent.click(screen.getByLabelText("Remember on this device"));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(localStorage.getItem(TOKEN_KEY)).toBe("tab");
    expect(sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it("names the header button in the 401 hint", async () => {
    mockApi({
      "api/v1/fleet": () => jsonResponse(401, { error: "invalid or missing bearer token" }),
    });
    render(<App />);
    const hint = await screen.findByText(/requires a read token/);
    const label = tokenButton().textContent!.replace(/[○●]/g, "").trim();
    expect(hint.textContent).toContain(`“${label}”`);
  });
});

describe("navigation", () => {
  it("routes #/teams to the fleet team rollup", async () => {
    mockApi({
      "api/v1/fleet": { targets: [], clusters: [] },
    });
    navigate("#/teams");
    render(<App />);
    expect(await screen.findByRole("heading", { level: 1, name: "Teams" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Teams" }).getAttribute("aria-current")).toBe("page");
  });
});
