import { describe, expect, it } from "vitest";
import { githubDevicePollStateAfterFailure } from "./SettingsPane";

describe("githubDevicePollStateAfterFailure", () => {
  it("keeps a device flow retryable until three consecutive failures", () => {
    expect(githubDevicePollStateAfterFailure(1)).toBe("pending");
    expect(githubDevicePollStateAfterFailure(2)).toBe("pending");
    expect(githubDevicePollStateAfterFailure(3)).toBe("failed");
  });
});
