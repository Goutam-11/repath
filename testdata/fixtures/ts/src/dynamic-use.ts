const tools = require("./util");
const loaded = require("./barrel");

export function dynamic(value: number): number {
  const viaTools = tools.add(value, 1);
  const viaBarrel = loaded.bound(viaTools);
  return viaBarrel;
}

export async function dynamicAwait(value: number): Promise<number> {
  const later = await import("./util");
  return later.clamp(value);
}
