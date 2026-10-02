// @vitest-environment happy-dom
import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { UnrecognizedImages } from "./UnrecognizedImages";
import "./test-utils";

describe("UnrecognizedImages", () => {
  it("counts every unrecognized image and lists the ones the report carries", () => {
    render(
      <UnrecognizedImages
        images={["corp.example/edge/nginx-controller", "docker.io/library/redis"]}
        omitted={3}
      />,
    );
    const section = screen.getByRole("region", { name: "Unrecognized images (5)" });
    const items = within(section)
      .getAllByRole("listitem")
      .map((li) => li.textContent);
    expect(items).toEqual(["corp.example/edge/nginx-controller", "docker.io/library/redis"]);
    expect(within(section).getByText(/found only through its labels or Helm release/)).toBeTruthy();
    expect(within(section).getByText("…and 3 more, not listed.")).toBeTruthy();
  });

  it("renders nothing when every image was recognized", () => {
    const { container } = render(<UnrecognizedImages images={undefined} omitted={undefined} />);
    expect(container.innerHTML).toBe("");
  });
});
