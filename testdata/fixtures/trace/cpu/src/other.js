"use strict";

// A second `greet` at a different file and line: a name-only matcher would
// attach the trace frame here. Mapping is file + line + name only.
function greet(name) {
  return `other:${name}`;
}

module.exports = { greet };
