import { spin } from "@demo/gadget";
import { gadget } from "@demo/widget";
import { render } from "@demo/web";
// Negated workspace package: must stay external, never a name guess.
import { old } from "@demo/legacy";
// Directory no workspace pattern matches: also external.
import { stray } from "@demo/unlisted";

export function run(): number {
  return spin() + gadget() + render() + old() + stray();
}
