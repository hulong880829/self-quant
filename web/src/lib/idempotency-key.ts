const UUID_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

function formatUUIDv4(bytes: Uint8Array): string {
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

function uuidFromRandomValues(): string | null {
  const cryptoObj = globalThis.crypto;
  if (typeof cryptoObj?.getRandomValues !== "function") {
    return null;
  }
  const bytes = new Uint8Array(16);
  cryptoObj.getRandomValues(bytes);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  return formatUUIDv4(bytes);
}

function uuidFallback(): string {
  const hex = (length: number) =>
    Array.from({ length }, () => Math.floor(Math.random() * 16).toString(16)).join("");
  const timestamp = Date.now().toString(16).padStart(12, "0").slice(-12);
  const variant = ["8", "9", "a", "b"][Math.floor(Math.random() * 4)];
  return `${timestamp.slice(0, 8)}-${timestamp.slice(8)}-4${hex(3)}-${variant}${hex(3)}-${hex(12)}`;
}

export function createIdempotencyKey(): string {
  const randomUUID = globalThis.crypto?.randomUUID;
  if (typeof randomUUID === "function") {
    return randomUUID.call(globalThis.crypto);
  }
  return uuidFromRandomValues() ?? uuidFallback();
}

export function isUUID(value: string): boolean {
  return UUID_PATTERN.test(value);
}
