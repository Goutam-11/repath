// Barrel: every export is re-exported from another module, which is the shape
// that used to resolve as pending.
export * from "./util";
export { clamp as bound } from "./util";
export { Cart as ShoppingCart } from "./cart";
