const errorCodeLabels: Readonly<Record<string, string>> = {
  E_NOT_FOUND: "文件不存在",
  E_BAD_PATH: "路径无效",
  E_PATH_DENIED: "路径被拒绝",
  E_NOT_CONFIRMED: "未确认",
  E_READONLY: "只读文件",
  E_ACCESS_DENIED: "访问被拒绝",
  E_DELETE_FAILED: "删除失败",
  E_RECYCLE_FAILED: "移入回收站失败",
  E_IN_USE: "文件正在使用",
  E_REPARSE: "重解析点被拒绝",
  E_BAD_MODE: "删除模式无效",
  E_HELPER_LOST: "Helper 连接丢失"
};

/** 删除错误码 → "CODE（中文说明）"；未知码回退原文，缺失码回退占位。 */
export function errorCodeText(code: string | undefined): string {
  if (!code) return "未提供";
  const label = errorCodeLabels[code];
  return label ? `${code}（${label}）` : code;
}
