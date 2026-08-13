import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('./loadpoints.js', import.meta.url), 'utf8');

function sliceFn(name, next) {
  const start = source.indexOf('function ' + name);
  assert.ok(start >= 0, name + ' must exist');
  const end = next ? source.indexOf('function ' + next, start + 1) : source.length;
  assert.ok(end > start, name + ' must be followed by ' + next);
  return source.slice(start, end);
}

test('escapeHtml exists and matches the heating.js mapping', () => {
  assert.match(source, /function escapeHtml/);
  assert.match(source, /'&': '&amp;'/);
  assert.match(source, /'<': '&lt;'/);
  assert.match(source, /'>': '&gt;'/);
  assert.match(source, /'"': '&quot;'/);
  assert.match(source, /"'": '&#39;'/);
});

test('loadpointCard escapes lp.id on the card wrapper', () => {
  const card = sliceFn('loadpointCard', 'render');
  assert.match(card, /data-lp-id="\$\{escapeHtml\(lp\.id\)\}"/);
  assert.match(card, /<h3>\$\{escapeHtml\(lp\.id\)\}<\/h3>/);
  assert.doesNotMatch(card, /data-lp-id="\$\{lp\.id\}"/);
  assert.doesNotMatch(card, /\$\{lp\.id\}/);
});

test('planner reason text is escaped before it hits the table', () => {
  assert.match(source, /escapeHtml\(a\.reason \|\| ''\)/);
});

test('vehicle driver and charging state are escaped', () => {
  assert.match(source, /escapeHtml\(lp\.vehicle_driver\)/);
  assert.match(source, /escapeHtml\(lp\.vehicle_charging_state\)/);
});

test('planLpId is escaped when shown in the empty-state copy', () => {
  assert.match(source, /escapeHtml\(planLpId\)/);
});
