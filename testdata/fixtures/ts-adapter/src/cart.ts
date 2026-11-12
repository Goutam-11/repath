export class Cart {
  total(): number {
    return 1;
  }
}

export function make(): Cart {
  return new Cart();
}
