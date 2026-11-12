import express from "express";

const app = express();

export function start(): void {
  step();
}

function step(): void {
  finish();
}

function finish(): void {}

app.get("/start", start);
