import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(webRoot, "index.html"), "utf8");
const router = readFileSync(join(webRoot, "diagnose.js"), "utf8");
const css = readFileSync(join(webRoot, "app.css"), "utf8");
const more = html.match(/<main id="view-more"[\s\S]*?<\/main>/)?.[0] || "";

describe("More destination layout", () => {
  it("puts everyday actions before advanced diagnostics", () => {
    const actionsAt = more.indexOf('class="more-actions"');
    const advancedAt = more.indexOf('id="more-advanced"');
    assert.ok(actionsAt > 0 && advancedAt > actionsAt,
      "quick actions should precede the advanced section");
  });

  it("names the advanced block in plain language", () => {
    assert.match(more, /id="more-advanced-title"/);
    assert.match(more, />Advanced diagnostics</);
    assert.match(more, /Turn on Advanced below/);
    assert.match(more, /id="more-advanced-mount"/);
  });

  it("keeps the three primary shortcuts obvious on mobile and desktop", () => {
    for (const id of ["more-settings-btn", "more-update-btn"]) {
      assert.match(more, new RegExp(`id="${id}"`));
    }
    assert.match(more, /href="\/setup"/);
    assert.match(css, /body\.ftw-app \.more-action \{[\s\S]*?min-height: 76px/);
    assert.match(css, /@media \(max-width: 720px\)[\s\S]*body\.ftw-app \.more-actions \{ grid-template-columns: 1fr; \}/);
  });

  it("routes advanced sections into the dedicated mount", () => {
    assert.match(router, /getElementById\('more-advanced-mount'\)/);
    assert.match(router, /forEach\(selector => append\(moreMount, selector\)\)/);
    assert.match(router, /'#ui-mode-row', '#twins-section'/);
    assert.match(router, /more-settings-btn[\s\S]*settings\.click\(\)/);
    assert.match(router, /more-update-btn[\s\S]*badge\.open\(\)/);
  });
});
