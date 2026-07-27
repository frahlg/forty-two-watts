import assert from "node:assert/strict";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, it } from "node:test";

const webRoot = new URL(".", import.meta.url).pathname;
const theme = readFileSync(join(webRoot, "components/theme.css"), "utf8");

// Tokens declared in a given selector's block, as {name: value}.
function tokensIn(selector) {
  const start = theme.indexOf(selector + " {");
  assert.notEqual(start, -1, `${selector} block not found in theme.css`);
  const end = theme.indexOf("\n}", start);
  const block = theme.slice(start, end);
  const out = {};
  for (const m of block.matchAll(/^\s*(--[a-z0-9-]+):\s*([^;]+);/gm)) {
    out[m[1]] = m[2].trim();
  }
  return out;
}

// A value that paints something: hex, oklch, rgb, or a var() pointing at
// another token. Sizes and bare numbers are theme-independent.
function isColour(value) {
  return /^(#|oklch\(|rgb|hsl|color-mix\(|var\(--)/.test(value);
}

function sourceFiles(dir, acc = []) {
  for (const entry of readdirSync(dir)) {
    if (entry === "node_modules" || entry === "vendor") continue;
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) sourceFiles(path, acc);
    else if (/\.(js|css|html|mjs)$/.test(entry)) acc.push(path);
  }
  return acc;
}

const allSource = sourceFiles(webRoot)
  .filter((p) => !p.endsWith("theme-tokens.test.mjs"))
  .map((p) => readFileSync(p, "utf8"))
  .join("\n");

describe("theme tokens", () => {
  const dark = tokensIn(":root");
  const light = tokensIn('html[data-theme="light"]');

  // The base palette (--bg, --surface, --border, --text, …) predates the
  // oklch palette and was declared only on :root. Every use of it therefore
  // painted a dark value in light mode — near-white --text on paper,
  // dark-violet --border rules — across roughly 250 call sites.
  it("redefines every colour token that light mode actually uses", () => {
    const missing = [];
    for (const [name, value] of Object.entries(dark)) {
      if (!isColour(value)) continue;
      if (name in light) continue;
      // Only tokens something reads are a problem; an unused declaration
      // is dead weight, not a rendering bug.
      const uses = allSource.split(`var(${name})`).length - 1 +
                   (allSource.split(`var(${name},`).length - 1);
      if (uses > 0) missing.push(`${name} (${uses} uses, dark value ${value})`);
    }
    assert.deepEqual(missing, [], "colour tokens stuck at their dark value in light mode");
  });

  it("maps the legacy base palette onto the oklch palette in light mode", () => {
    // Mapping rather than hand-picking hexes keeps the two systems from
    // drifting; if someone retunes --line, --border follows.
    for (const [legacy, expected] of Object.entries({
      "--bg": "var(--ink)",
      "--surface": "var(--ink-raised)",
      "--surface2": "var(--ink-sunken)",
      "--border": "var(--line)",
      "--text": "var(--fg)",
      "--text-dim": "var(--fg-muted)",
    })) {
      assert.equal(light[legacy], expected, `${legacy} should map to ${expected}`);
    }
  });

  it("keeps the dark base palette on its own literal values", () => {
    // Deliberate: the legacy set is lighter and more violet than the oklch
    // set, so mapping it in dark mode too would restyle every surface. That
    // is a design change and does not belong in a bug fix.
    assert.match(dark["--surface"], /^#/);
    assert.match(dark["--border"], /^#/);
  });
});
