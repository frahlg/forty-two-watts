import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";

const url = (p) => new URL(p, import.meta.url);
const chart = readFileSync(url("./components/ftw-price-chart.js"), "utf8");
const outlook = readFileSync(url("./components/ftw-price-outlook.js"), "utf8");
const plan = readFileSync(url("./plan.js"), "utf8");
const html = readFileSync(url("./index.html"), "utf8");
const componentIndex = readFileSync(url("./components/index.js"), "utf8");

// One price definition across the app. The price card used to compute
// spot × 1.25 while calling it "incl. VAT", so a slot the plan chart
// priced at 109 öre showed as 21 öre on the same dashboard.
describe("every price surface uses the consumer total", () => {
  it("routes the price card and the outlook through the shared formula", () => {
    for (const [name, src] of [["price chart", chart], ["outlook", outlook]]) {
      assert.match(src, /from "\.\/price-math\.js"/, `${name} imports price-math`);
      assert.match(src, /consumerTotalOre\(/, `${name} calls consumerTotalOre`);
    }
  });

  it("does not re-derive the total inline in either component", () => {
    for (const [name, src] of [["price chart", chart], ["outlook", outlook]]) {
      assert.doesNotMatch(
        src, /item\.spot \* \(1 \+ this\._vatPct/,
        `${name} must not apply VAT to bare spot`,
      );
    }
  });

  it("reads the grid tariff from config, not just VAT", () => {
    for (const [name, src] of [["price chart", chart], ["outlook", outlook]]) {
      assert.match(src, /grid_tariff_ore_kwh/, `${name} reads the tariff`);
    }
  });

  it("offers total and spot rather than a VAT switch", () => {
    assert.match(chart, /data-price-mode="total"/);
    assert.match(chart, /data-price-mode="spot"/);
    assert.doesNotMatch(chart, /data-vat=/);
    // The header names the tariff when there is one, so the number is
    // never mistaken for spot-plus-VAT.
    assert.match(chart, /incl\. grid tariff \+ VAT/);
  });

  it("carries the operator's stored preference over from the old key", () => {
    assert.match(chart, /ftw\.priceChart\.totalOn/);
    assert.match(chart, /LEGACY_PREF_KEY = "ftw\.priceChart\.vatOn"/);
  });
});

// The plan chart drew its bars from plan.actions (the full horizon,
// including ML-predicted slots) but scaled them off state.prices
// (published slots only). Predicted slots above the known maximum were
// drawn past the top of the price band, and all of them landed above p75
// so the whole forecast read as expensive.
describe("plan chart price scale", () => {
  it("derives the scale from the same rows it draws", () => {
    assert.match(plan, /const barSource = \(plan && plan\.actions && plan\.actions\.length\) \? plan\.actions : prices;/);
    assert.match(plan, /const totals = priceBars\.map\(b => b\.total_ore_kwh \?\? b\.price_ore\);/);
    // The scale must come after the bar set is known, so the old
    // prices-only derivation cannot come back by accident.
    assert.ok(
      plan.indexOf("const priceBars =") < plan.indexOf("const priceMin ="),
      "priceBars is established before the scale",
    );
    assert.doesNotMatch(plan, /const totals = prices\.map\(p => p\.total_ore_kwh\)/);
  });

  it("iterates the filtered bar set when drawing", () => {
    assert.match(plan, /for \(const bar of priceBars\)/);
  });

  it("clips the price band so no slot can paint over the other bands", () => {
    assert.match(plan, /const clipPriceBand = \(\) => \{/);
    assert.match(plan, /ctx\.rect\(pad\.l, priceY0, plotW, priceH\)/);
    assert.match(plan, /clipPriceBand\(\);/);
  });

  it("marks predicted slots with a cap that survives 15-minute bars", () => {
    // A dashed frame per slot merged into a solid hatched wall at 96
    // slots a day; the cap is drawn regardless of bar width.
    assert.match(plan, /ctx\.fillRect\(rectX, Math\.min\(topY, zero\), rectW, 1\.5\)/);
    assert.match(plan, /if \(rectW >= 6\)/);
  });
});

// Direction against the average carries cheap-vs-dear, because the
// theme's green and red are ΔE 2.4 apart under deuteranopia in light
// mode — a colour-only strip would be unreadable for many readers.
describe("overview price outlook", () => {
  it("plots distance from the mean, not the raw total", () => {
    assert.match(outlook, /const devs = totals\.map\(\(t\) => t - mean\);/);
  });

  it("gives an öre the same pixels above and below the line", () => {
    // One span for both arms, so a 20 öre saving is as tall as a 20 öre
    // surcharge. The line is then placed by the ratio of the extremes
    // rather than at mid-height, so neither arm is squashed into a stub.
    assert.match(outlook, /const span = Math\.max\(up \+ down, 1\);/);
    assert.match(outlook, /const midY = pad\.t \+ \(up \/ span\) \* plotH;/);
    assert.match(outlook, /const devToPx = \(d\) => \(d \/ span\) \* plotH;/);
  });

  it("responds to the card's own width, not the window's", () => {
    // A media query reports the viewport, so a narrow column on a wide
    // screen kept the desktop layout and the text overflowed its tile.
    assert.match(outlook, /new ResizeObserver\(/);
    assert.match(outlook, /const small = W < 480;/);
    assert.doesNotMatch(outlook, /matchMedia/);
    assert.doesNotMatch(outlook, /@media/);
    // The tile count follows the container, which plain CSS can do.
    assert.match(outlook, /repeat\(auto-fit, minmax\(150px, 1fr\)\)/);
  });

  it("draws the chart in real pixels so labels keep their size", () => {
    // A fixed 1000-unit viewBox scaled the whole drawing — text included —
    // with the card, shrinking axis labels to a few pixels when narrow.
    assert.match(outlook, /const W = this\._width > 0 \? this\._width : 640;/);
    assert.doesNotMatch(outlook, /preserveAspectRatio/);
    // Tick density comes from the pixels available per label.
    assert.match(outlook, /const minTickPx = \d+;/);
  });

  it("guards the resize observer against re-render feedback", () => {
    assert.match(outlook, /Math\.abs\(w - this\._width\) > 4/);
    assert.match(outlook, /this\._ro\.disconnect\(\)/);
  });

  it("asks for an hour of lookback so the current slot is present", () => {
    // /api/prices filters on the slot's start time, so since_ms=now drops
    // the slot in progress — the one the Now tile reports.
    assert.match(outlook, /since_ms=\$\{now - 3600_000\}/);
    assert.match(outlook, /\.filter\(\(it\) => it\.tsMs \+ it\.lenMin \* 60_000 > now\)/);
  });

  it("states the finding in words and glyphs, not only in colour", () => {
    assert.match(outlook, /Cheapest \$\{BLOCK_H\} h/);
    assert.match(outlook, /Priciest \$\{BLOCK_H\} h/);
    assert.match(outlook, /class="tile-arrow" aria-hidden="true">\$\{nowArrow\}/);
    assert.match(outlook, /role="img" aria-label=/);
  });

  it("labels the baseline with the average it represents", () => {
    assert.match(outlook, /avg \$\{roundOre\(mean\)\} öre/);
  });

  it("links to the full chart instead of duplicating it", () => {
    assert.match(outlook, /href="#energy"/);
  });

  it("is registered and mounted on Overview", () => {
    assert.match(componentIndex, /import "\.\/ftw-price-outlook\.js/);
    const overview = html.match(/<main id="view-overview"[\s\S]*?<main id="view-energy"/)?.[0] || "";
    assert.match(overview, /<ftw-price-outlook><\/ftw-price-outlook>/);
    assert.equal((html.match(/<ftw-price-outlook\b/g) || []).length, 1);
  });

  it("stays on Overview — the destination router must not move it", () => {
    const router = readFileSync(url("./diagnose.js"), "utf8");
    assert.doesNotMatch(router, /price-outlook-row/);
  });
});
