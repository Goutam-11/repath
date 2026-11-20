// #82: a destructured prop callback. `onClick` is a parameter, not a symbol,
// so the only repo-wide declaration is Clipboard.onClick in another file.
export function Button(onClick: () => void): void {
  onClick();
}
