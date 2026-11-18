export class Left {
  total(): number { return 1; }
}

export class Right {
  total(): number { return 2; }
}

export function makeLeft() { return new Left(); }
export function makeRight() { return new Right(); }
