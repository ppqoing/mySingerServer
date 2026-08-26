import { describe, expect, it } from "vitest";
import { ApiError } from "./client";
import { apiErrorText, databaseErrorText } from "./errorText";

describe("apiErrorText", () => {
  it("maps postgres error codes to guided Chinese text", () => {
    expect(apiErrorText(new ApiError(503, "postgres_not_configured", true))).toContain("未配置数据库");
    expect(apiErrorText(new ApiError(503, "postgres_auth_failed", true))).toContain("认证失败");
    expect(apiErrorText(new ApiError(503, "postgres_unreachable", true))).toContain("无法连接数据库");
    expect(apiErrorText(new ApiError(503, "postgres_unavailable", true))).toContain("恢复连接");
  });

  it("maps manager restart and shutdown states", () => {
    expect(apiErrorText(new ApiError(503, "server_shutting_down", true))).toContain("正在重启");
    expect(apiErrorText(new Error("Manager restart timed out"))).toContain("重启后监听失败");
  });

  it("maps delete flow errors", () => {
    expect(apiErrorText(new ApiError(409, "delete selection conflict", false))).toContain("选择冲突");
    expect(apiErrorText(new ApiError(404, "delete task not found", false))).toContain("重启清除");
  });

  it("maps invalid URL and agent offline", () => {
    expect(apiErrorText(new TypeError("Invalid URL"))).toContain("恢复地址无效");
    expect(apiErrorText(new ApiError(503, "agent_offline", true))).toContain("Agent 离线");
  });

  it("falls back to the original message and then to the fallback", () => {
    expect(apiErrorText(new ApiError(500, "自定义错误", true))).toBe("自定义错误");
    expect(apiErrorText(new Error(""), "兜底")).toBe("兜底");
    expect(apiErrorText("not-an-error", "兜底")).toBe("兜底");
  });
});

describe("databaseErrorText", () => {
  it("maps known codes", () => {
    expect(databaseErrorText("postgres_not_configured")).toContain("未配置数据库");
    expect(databaseErrorText("postgres_unreachable")).toContain("无法连接数据库");
  });

  it("returns generic text when code is missing or unknown", () => {
    expect(databaseErrorText(undefined)).toContain("恢复连接");
    expect(databaseErrorText("some_future_code")).toBe("some_future_code");
  });
});
