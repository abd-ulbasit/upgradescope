// @vitest-environment happy-dom
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import "./test-utils";
import { Citations } from "./ui";

describe("Citations", () => {
  it("renders a repeated URL twice without a duplicate React key", () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    const url = "https://kubernetes.io/docs/reference/using-api/deprecation-guide/";
    render(<Citations urls={[url, url]} />);
    const links = screen.getAllByRole("link");
    expect(links.map((a) => a.textContent)).toEqual([
      "kubernetes.io › deprecation-guide",
      "kubernetes.io › deprecation-guide (2)",
    ]);
    expect(errors).not.toHaveBeenCalled();
  });
});
