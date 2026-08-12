export class ApiError extends Error {
  readonly status: number;
  readonly retryable: boolean;

  constructor(status: number, message: string, retryable: boolean) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.retryable = retryable;
  }
}

export interface JsonRequestOptions extends RequestInit {
  allowNoContent?: boolean;
  decodeStatuses?: readonly number[];
}

export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException
    ? error.name === "AbortError"
    : typeof error === "object" && error !== null && "name" in error &&
      (error as { name?: unknown }).name === "AbortError";
}

export async function requestJson<T>(
  url: string,
  options: JsonRequestOptions,
  decode: (value: unknown, status?: number) => T
): Promise<T> {
  let response: Response;
  try {
    response = await fetch(url, fetchOptions(options));
  } catch (error) {
    if (isAbortError(error)) {
      throw error;
    }
    throw new ApiError(0, "网络请求失败", true);
  }

  const value = await parseResponse(response, false, options.decodeStatuses);
  try {
    return decode(value, response.status);
  } catch (error) {
    if (error instanceof ApiError || isAbortError(error)) {
      throw error;
    }
    throw new ApiError(response.status, "服务返回的数据无效", retryable(response.status));
  }
}

export async function requestVoid(
  url: string,
  options: JsonRequestOptions
): Promise<void> {
  let response: Response;
  try {
    response = await fetch(url, fetchOptions(options));
  } catch (error) {
    if (isAbortError(error)) {
      throw error;
    }
    throw new ApiError(0, "网络请求失败", true);
  }
  await parseResponse(response, options.allowNoContent === true);
}

async function parseResponse(
  response: Response,
  allowNoContent: boolean,
  decodeStatuses: readonly number[] = []
): Promise<unknown> {
  if (response.status === 204) {
    if (allowNoContent) {
      return undefined;
    }
    throw new ApiError(response.status, "服务返回的数据无效", false);
  }

  const raw = await response.text();
  let value: unknown;
  try {
    value = raw === "" ? undefined : JSON.parse(raw);
  } catch {
    throw new ApiError(response.status, response.ok ? "服务返回的数据无效" : "请求失败", retryable(response.status));
  }

  if (!response.ok && !decodeStatuses.includes(response.status)) {
    const message = isRecord(value) && typeof value.error === "string" && value.error !== ""
      ? value.error
      : "请求失败";
    throw new ApiError(response.status, message, retryable(response.status));
  }
  if (value === undefined) {
    throw new ApiError(response.status, "服务返回的数据无效", retryable(response.status));
  }
  return value;
}

function fetchOptions(options: JsonRequestOptions): RequestInit {
  const requestOptions = { ...options };
  delete requestOptions.allowNoContent;
  delete requestOptions.decodeStatuses;
  return requestOptions;
}

function retryable(status: number): boolean {
  return status === 408 || status === 429 || status >= 500;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
