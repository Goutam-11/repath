import { clamp } from "@/util";

export class Cart {
  add(item: number): number {
    return clamp(item);
  }

  total(): number {
    return this.add(1);
  }
}
