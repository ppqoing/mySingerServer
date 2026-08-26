import { ApiError } from "./client";

/**
 * 集中把后端原始错误码/英文错误文本映射为用户可读的中文文案与引导。
 * 匹配基于 ApiError.message（后端 body.error 原样透传），未知错误回退原文或 fallback。
 */
const errorTextRules: ReadonlyArray<{ readonly match: RegExp; readonly text: string }> = [
  { match: /postgres_not_configured/, text: "未配置数据库：请在 GUI 设置中填写 PostgreSQL 连接串（DSN），配置前不会自动恢复。" },
  { match: /postgres_auth_failed/, text: "数据库认证失败：请检查 PostgreSQL 用户名和密码。" },
  { match: /postgres_unreachable/, text: "无法连接数据库：请检查网络与数据库服务状态。" },
  { match: /postgres_unavailable/, text: "数据库暂不可用：Manager 会继续尝试恢复连接。" },
  { match: /server_shutting_down/, text: "Manager 正在重启，稍后自动恢复。" },
  { match: /delete selection conflict/i, text: "选择冲突：部分文件已在其他删除任务中，请调整选择后重试。" },
  { match: /delete task not found/i, text: "删除任务不存在或已随 Manager 重启清除。" },
  { match: /Manager restart timed out/i, text: "重启后监听失败，请检查 data\\logs\\gui.log。" },
  { match: /Invalid URL/i, text: "恢复地址无效：请检查配置中的监听/恢复地址。" },
  { match: /agent_offline/, text: "Agent 离线：请确认目标节点在线后重试。" }
];

/** 把后端错误翻译为用户可读文案；识别不了时返回原始 message（本身多为后端中文）。 */
export function apiErrorText(error: unknown, fallback = "请求失败"): string {
  const message = error instanceof Error && error.message ? error.message : fallback;
  for (const rule of errorTextRules) {
    if (rule.match.test(message)) {
      return rule.text;
    }
  }
  return message;
}

/** 数据库错误码（RuntimeStatus.databaseErrorCode）的展示文案；无码时返回通用文案。 */
export function databaseErrorText(code: string | undefined): string {
  if (!code) {
    return "业务数据暂不可用，Manager 会继续尝试恢复连接。";
  }
  return apiErrorText(new ApiError(503, code, true));
}
