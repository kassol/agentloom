import { describe, expect, it } from "vitest";
import { activityIntensity, shiftCalendarDate } from "./daily-activity";

describe("daily activity helpers", () => {
  it("shifts dates across month boundaries in local calendar time", () => {
    expect(shiftCalendarDate("2026-07-01", -1)).toBe("2026-06-30");
    expect(shiftCalendarDate("2026-12-31", 1)).toBe("2027-01-01");
  });

  it("maps execution minutes to stable half-hour intensity bands", () => {
    expect(activityIntensity(0)).toBe(0);
    expect(activityIntensity(9 * 60)).toBe(1);
    expect(activityIntensity(10 * 60)).toBe(2);
    expect(activityIntensity(20 * 60)).toBe(3);
  });
});
