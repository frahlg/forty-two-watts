import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
import { describe, it } from "node:test";

const source = readFileSync(new URL("./weather.js", import.meta.url), "utf8");

describe("weather PV array nameplate", () => {
  it("binds Rated (W) to rated_w, not kwp", () => {
    assert.match(source, /Rated \(W\)/);
    assert.match(source, /data-field="rated_w"/);
    assert.match(source, /placeholder="12960"/);
    assert.doesNotMatch(source, /data-field="kwp"/);
    assert.match(source, /rated_w: 0/);
    assert.doesNotMatch(source, /kwp: 0/);
  });

  it("converts a leftover kwp into rated_w", () => {
    assert.match(source, /function ratedWattsFromLegacyKwp/);
    assert.match(source, /v >= 1000 \? v : v \* 1000/);
    assert.match(source, /function migrateArrayRatedW/);
    assert.match(source, /delete a\.kwp/);
  });
});
