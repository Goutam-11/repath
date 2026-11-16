import { gadget } from "@fixture/widget";
import { spin } from "@fixture/gizmo";

export function viaWorkspace(value: number): number {
  return gadget(spin(value));
}
