import { describe, expect, it } from "vitest";
import { EDGE_CONTROL_SIZE_TOLERANCE, edgeControlMeetsMinimum } from "./team-graph-controls";

describe("edgeControlMeetsMinimum", () => {
  it("accepts normal subpixel rasterization around the minimum", () => {
    expect(edgeControlMeetsMinimum(43.9999, 44, 44)).toBe(true);
  });

  it("rejects a control that is smaller than the explicit tolerance", () => {
    expect(edgeControlMeetsMinimum(44 - EDGE_CONTROL_SIZE_TOLERANCE - 0.001, 44, 44)).toBe(false);
    expect(edgeControlMeetsMinimum(44, 44 - EDGE_CONTROL_SIZE_TOLERANCE - 0.001, 44)).toBe(false);
  });
});
