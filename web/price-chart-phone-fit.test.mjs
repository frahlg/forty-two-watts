// node --test web/price-chart-phone-fit.test.mjs
//
// How much of a phone the price chart takes, and whether its y axis fits
// inside its own SVG.
//
// The repo ships no DOM polyfill, so this is a structural test over the
// source — the approach price-zone-picker.test.mjs and setup.test.mjs take.
// The behavioural half, which mounts the component and reads the SVG it
// writes, lives in the FTW app beside its copy of this file
// (tests/price-chart-phone.test.ts there); the app is where a browser
// environment already exists. What is worth having here is that the box's
// own CI notices if either number goes back.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const chart = readFileSync(join(webRoot, "components", "ftw-price-chart.js"), "utf8");

/** The one place the viewBox height is decided. */
const heightLine = chart.match(/^\s*const H = .*$/m)?.[0] ?? "";
/** The phone's y-axis font size, in viewBox units. */
const fsAxisPhone = Number(chart.match(/const fsAxis = small \? (\d+)/)?.[1]);
/** The monospace advance the gutter is sized with, in ems. */
const monoAdvance = Number(chart.match(/const MONO_ADVANCE_EM = ([\d.]+)/)?.[1]);
/** The gutter floor on the phone branch of the padding object. */
const phoneGutterFloor = Number(
  chart.match(/\{ t: 26, r: 16, b: 40, l: Math\.max\((\d+), gutter\) \}/)?.[1]
);

describe("the price chart on a phone", () => {
  it("is shorter in the app than on the dashboard", () => {
    // The dashboard's Energy tab is a page about prices and the chart is what
    // you came for. The app's Plan screen puts a timeline under it, and at the
    // dashboard's shape the chart alone filled a phone. `fed` is how this file
    // already tells the two apart.
    assert.match(heightLine, /hasAttribute\("fed"\)/,
      "the viewBox height no longer distinguishes the app from the dashboard");

    const [appH, dashboardH] = heightLine.match(/\? (\d+) : (\d+)\)/).slice(1).map(Number);
    assert.equal(dashboardH, 720, "the dashboard's phone height moved");
    assert.ok(appH < dashboardH * 0.7,
      `the app's phone height is ${appH}, not meaningfully shorter than ${dashboardH}`);
  });

  it("sizes the left gutter from the labels rather than guessing at them", () => {
    // Anchored "end" inside the gutter, a y label grows leftwards; a gutter
    // narrower than the longest one clipped its first character, and "0.00 ö"
    // came out as ".00 ö". A fixed number can only ever suit one font size,
    // one currency and one price range.
    assert.match(chart, /const gutter = 4 \+ Math\.ceil\(/);
    assert.match(chart, /yTicks\.map\(\(t\) => t\.text\.length\)/,
      "the gutter is no longer measured from the label text");

    assert.ok(Number.isFinite(monoAdvance) && monoAdvance >= 0.6,
      `a ${monoAdvance} em advance is narrower than any monospace face the --mono stack falls back to`);

    // The case that was clipped, checked as arithmetic rather than as text:
    // six characters of phone-sized axis, plus the four units the label is
    // inset by, have to fit in the gutter the phone branch settles on.
    const needed = 4 + Math.ceil("0.00 ö".length * monoAdvance * fsAxisPhone);
    assert.ok(needed > phoneGutterFloor,
      `the phone's ${phoneGutterFloor}-unit floor already covers "0.00 ö", so nothing here is being measured`);
  });
});
