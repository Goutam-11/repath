import { makeLeft, makeRight } from "./cart";
import { Dep } from "some-dep";

export function run(d: Dep) {
  const a = makeLeft(); const b = makeRight();
  a.total(); b.total();
  const c = makeLeft();
  c.total(); d.total();
}
