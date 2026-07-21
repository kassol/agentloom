import { describe, expect, it } from "vitest";
import { moveItem, reorderItem } from "./tab-order";

describe("agent tab ordering", () => {
  it("places a tab before or after the target", () => {
    expect(reorderItem(["a", "b", "c", "d"], "d", "b", "before")).toEqual(["a", "d", "b", "c"]);
    expect(reorderItem(["a", "b", "c", "d"], "a", "c", "after")).toEqual(["b", "c", "a", "d"]);
  });

  it("preserves the original array when the order does not change", () => {
    const original = ["a", "b", "c"];
    expect(reorderItem(original, "b", "c", "before")).toBe(original);
    expect(reorderItem(original, "missing", "c", "before")).toBe(original);
  });

  it("supports bounded keyboard moves", () => {
    expect(moveItem(["a", "b", "c"], "b", -1)).toEqual(["b", "a", "c"]);
    expect(moveItem(["a", "b", "c"], "b", 1)).toEqual(["a", "c", "b"]);
    const original = ["a", "b", "c"];
    expect(moveItem(original, "a", -1)).toBe(original);
  });
});
