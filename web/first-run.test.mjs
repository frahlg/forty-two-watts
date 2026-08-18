// node --test web/first-run.test.mjs
//
// First-run / empty-home UX. The dashboard used to inject "No devices"
// after .summary-cards, which now lives in the hidden Values panel — so
// the default Flow view never showed a way into setup.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(webRoot, "index.html"), "utf8");
const app = readFileSync(join(webRoot, "app.js"), "utf8");
const css = readFileSync(join(webRoot, "app.css"), "utf8");
const overview = html.match(/<main id="view-overview"[\s\S]*?<\/main>/)?.[0] || "";

describe("first-run empty state is visible on Overview", () => {
  it("ships a hidden empty card on Overview, not inside Values", () => {
    assert.match(overview, /id="overview-empty"/);
    assert.match(overview, /id="overview-empty"[^>]*hidden/);
    assert.match(overview, /href="\/setup\?step=3"/);
    assert.match(overview, /Run setup wizard/);
    assert.match(overview, /The wizard replaces the current setup/);

    const emptyAt = overview.indexOf('id="overview-empty"');
    const valuesAt = overview.indexOf('id="power-now-values"');
    assert.ok(emptyAt > 0 && valuesAt > emptyAt,
      "the empty card must precede the hidden Values panel");
  });

  it("does not inject the prompt after the hidden summary-cards", () => {
    assert.doesNotMatch(
      app,
      /querySelector\(["']\.summary-cards["']\)/,
      "summary-cards is inside hidden Values — do not mount the prompt there",
    );
    assert.match(app, /getElementById\(["']overview-empty["']\)/);
    assert.match(app, /classList\.toggle\(["']no-devices["'],\s*!hasDrivers\)/);
    assert.match(app, /empty\.hidden = hasDrivers/);
  });

  it("hides live instruments that only show dashes when no devices exist", () => {
    assert.match(css, /body\.ftw-app\.no-devices #power-now/);
    assert.match(css, /body\.ftw-app\.no-devices #overview-plan-summary/);
    assert.match(css, /body\.ftw-app\.no-devices #overview-today/);
    assert.match(css, /body\.ftw-app\.no-devices \.fuse-row/);
    assert.doesNotMatch(
      css,
      /body\.ftw-app\.no-devices #overview-price/,
      "price can exist before any device is added",
    );
  });

  it("labels the health chip No devices instead of Live", () => {
    assert.match(app, /overviewHealthLabel\.textContent = ["']No devices["']/);
    assert.match(app, /classList\.add\(["']is-empty["']\)/);
    assert.match(css, /\.overview-health\.is-empty/);
  });

  it("keeps the CTA honest about the wizard replacing config", () => {
    assert.match(overview, /The wizard replaces the current setup/);
    assert.doesNotMatch(
      overview,
      /Add a device/,
      "Save replaces config — do not imply an additive add-device flow",
    );
  });
});
