import { renderToString } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { Greeting, shout } from "./Greeting";

describe("Greeting", () => {
  it("shouts the name", () => {
    expect(shout("a")).toBe("A!");
    expect(renderToString(<Greeting name="ship" />)).toContain("SHIP!");
  });
});
