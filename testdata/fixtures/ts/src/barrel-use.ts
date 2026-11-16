import { add, bound, ShoppingCart } from "./barrel";
import format from "./default-barrel";

export function viaBarrel(a: number, b: number): number {
  const total = add(a, b);
  const capped = bound(total);
  const cart = new ShoppingCart();
  return format(capped).length + cart.total();
}
