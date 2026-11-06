import type { Shape } from "./shape";
import { Base } from "./base";

export class Circle extends Base implements Shape {
  area(): number {
    return 3;
  }
}
