import { clamp } from "./deep1";

export function capped(value: number): number {
  return clamp(value);
}
