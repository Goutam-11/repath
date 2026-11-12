import { helper } from "./helper";

export class Cart {
  total(): number {
    return helper();
  }
}

export function run(cart: Cart): number {
  return cart.total();
}
