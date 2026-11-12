import { make } from "./cart";

export function run(): number {
  const cart = make();
  return cart.total();
}
