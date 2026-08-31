import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const assistant = readFileSync(new URL("./assistant.js", import.meta.url), "utf8");
const index = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const system = readFileSync(new URL("./settings/tabs/system.js", import.meta.url), "utf8");
const appJs = readFileSync(new URL("./app.js", import.meta.url), "utf8");
const appCss = readFileSync(new URL("./app.css", import.meta.url), "utf8");
const issueTemplate = readFileSync(new URL("../.github/ISSUE_TEMPLATE/ask_why.md", import.meta.url), "utf8");

test("Ask why is wired from the plan card", () => {
  assert.match(index, /id="plan-ask-why"/);
  assert.match(index, /assistant\.js/);
  assert.match(assistant, /getElementById\("plan-ask-why"\)/);
  assert.match(assistant, /\/api\/assistant\/ask/);
  assert.match(assistant, /\/api\/assistant\/status/);
});

test("the plan briefing takes a question about the schedule", () => {
  assert.match(index, /id="plan-ask-why-form"/);
  assert.match(index, /id="plan-ask-why-plan"/);
  assert.match(index, /id="plan-ask-why-input"/);
  assert.match(assistant, /kind: "plan"/);
  assert.match(assistant, /Why did FTW plan the next hours this way\?/);
});

test("Ask why drafts a GitHub issue instead of posting one", () => {
  assert.match(assistant, /Open GitHub issue/);
  assert.match(assistant, /issues\/new\?template=ask_why\.md/);
  assert.doesNotMatch(assistant, /bug_report\.yml/);
  assert.doesNotMatch(assistant, /api\.github\.com/);
  assert.match(issueTemplate, /One text field/);
  assert.doesNotMatch(issueTemplate, /What happened/);
});

test("the conversation keeps the question and the answer in separate messages", () => {
  assert.match(assistant, /ftw-ask-msg user/);
  assert.match(assistant, /ftw-ask-msg assistant/);
  assert.match(assistant, /This can take a minute/);
  assert.match(assistant, /text\/event-stream/);
  assert.match(assistant, /innerHTML = formatAnswer/);
  assert.doesNotMatch(assistant, /innerHTML = j\.answer/);
});

test("settings hold the OpenRouter key on the box", () => {
  assert.match(system, /data-path="assistant.api_key"/);
  assert.match(system, /openrouter\.ai\/keys/);
});

test("the header chip sits outside the hamburger cluster", () => {
  const chipAt = index.indexOf('id="ask-why-chip"');
  const rightAt = index.indexOf('class="header-right"');
  assert.ok(chipAt > 0 && chipAt < rightAt, "chip must stay in the header row on phones");
  assert.match(appCss, /\.ask-why-chip/);
  assert.match(appCss, /outside \.header-right/);
  assert.match(appJs, /CustomEvent\("ftw-status"/);
  assert.match(assistant, /driver_offline/);
});

test("the offline chip names the driver and hides when it recovers", () => {
  const chip = {
    hidden: true,
    textContent: "",
    title: "",
    removeAttribute() { this.title = ""; },
    addEventListener() {},
  };
  const sandbox = {
    document: {
      readyState: "complete",
      getElementById: (id) => (id === "ask-why-chip" ? chip : null),
      addEventListener() {},
      createElement: () => ({ style: {}, setAttribute() {} }),
      head: { appendChild() {} },
      body: { appendChild() {} },
    },
    fetch: () => new Promise(() => {}),
    navigator: {},
    console,
    addEventListener() {},
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(assistant, sandbox);
  sandbox.window.FTWAskWhy.updateChip({
    drivers: { sungrow: { status: "offline" }, meter: { status: "ok" } },
  });
  assert.equal(chip.hidden, false);
  assert.match(chip.textContent, /sungrow is offline/);
  sandbox.window.FTWAskWhy.updateChip({
    drivers: { sungrow: { status: "ok" }, meter: { status: "ok" } },
  });
  assert.equal(chip.hidden, true);
});
