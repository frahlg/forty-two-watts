import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const assistant = readFileSync(new URL("./assistant.js", import.meta.url), "utf8");
const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const system = readFileSync(new URL("./settings/tabs/system.js", import.meta.url), "utf8");

test("Ask why is wired from the plan card", () => {
  assert.match(index, /id="plan-ask-why"/);
  assert.match(index, /assistant\.js/);
  assert.match(assistant, /getElementById\("plan-ask-why"\)/);
  assert.match(assistant, /\/api\/assistant\/ask/);
  assert.match(assistant, /\/api\/assistant\/status/);
});

test("Ask why drafts a GitHub issue instead of posting one", () => {
  assert.match(assistant, /Open GitHub issue/);
  assert.match(assistant, /issues\/new\?template=bug_report\.yml/);
  assert.doesNotMatch(assistant, /api\.github\.com/);
});

test("model output is assigned as text, not HTML", () => {
  assert.match(assistant, /answerEl\.textContent = j\.answer/);
  assert.doesNotMatch(assistant, /answerEl\.innerHTML = j\.answer/);
});

test("settings hold the OpenRouter key on the box", () => {
  assert.match(system, /data-path="assistant.api_key"/);
  assert.match(system, /openrouter\.ai\/keys/);
});
