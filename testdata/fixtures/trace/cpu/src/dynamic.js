"use strict";

function greet(name) {
  let out = "";
  for (let i = 0; i < 40; i++) out += name;
  return out;
}

function other(name) {
  return name.toUpperCase();
}

function dispatch(kind, name) {
  const handlers = { greet, other };
  return handlers[kind](name);
}

function run() {
  let total = 0;
  for (let i = 0; i < 20000; i++) {
    total += dispatch("greet", "x").length;
  }
  return total;
}

run();
