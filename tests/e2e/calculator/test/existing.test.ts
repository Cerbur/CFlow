// The calculator's existing unit tests (Node's built-in test runner,
// run through `npm test`; no npm dependencies).
import { test } from "node:test";
import assert from "node:assert/strict";
import { add } from "../src/add.ts";
import { subtract } from "../src/subtract.ts";

test("add returns the sum", () => {
  assert.equal(add(2, 3), 5);
});

test("subtract returns the difference", () => {
  assert.equal(subtract(5, 3), 2);
});
