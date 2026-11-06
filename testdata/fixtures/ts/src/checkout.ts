import { Cart } from "./cart";

export function checkout(): number {
  const cart = new Cart();
  return cart.total();
}
