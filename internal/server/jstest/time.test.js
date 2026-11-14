// renderTimes (web/app.js, design 4.7) names the zone of a timestamp in a tip, not in a native title: the <time> takes
// the focus and points at a hidden .tip-c, so a keyboard user sees it as the pointer user does (ui.md).
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const src = fs.readFileSync(path.join(__dirname, "..", "web", "app.js"), "utf8");
const a = src.indexOf("// ---- time:");
const b = src.indexOf("// wall:");
assert.ok(a > 0 && b > a, "the time section of app.js was not found");

function setup(zone) {
  const byId = {}, attrs = {};
  const document = { getElementById: (id) => byId[id] || null };
  const body = { dataset: { tz: zone }, appended: [], append(n) { this.appended.push(n); byId[n.attrs.id] = n; } };
  const el = (tag, at, ...kids) => ({ tag, attrs: at || {}, kids });
  const t = { textContent: "", dataset: {}, attrs, getAttribute: (k) => (k === "datetime" ? "2026-11-14T00:00:00Z" : attrs[k]), setAttribute(k, v) { attrs[k] = v; } };
  const h = { textContent: "(UTC)" };
  const $$ = (sel) => (sel === "[data-tzname]" ? [h] : sel.startsWith("time") ? [t] : []);
  const run = new Function("body", "el", "$$", "document", "Intl", src.slice(a, b) + "\nreturn { renderTimes, zoneTip };");
  return { api: run(body, el, $$, document, Intl), t, body, h };
}

test("a timestamp names its zone in a tip: focusable, no native title", () => {
  const p = setup("Asia/Tokyo");
  p.api.renderTimes();
  assert.strictEqual(p.t.textContent, "2026-11-14 09:00:00");
  assert.ok(!("title" in p.t), "a native title was set");
  assert.ok(!("title" in p.t.attrs), "a native title attribute was set");
  assert.ok(!("tabindex" in p.t.attrs), "a <time> must not be a Tab stop: a page holds hundreds");
  const id = p.t.dataset.tip;
  assert.ok(id, "no data-tip");
  const c = p.body.appended.find((n) => n.attrs.id === id);
  assert.ok(c && "hidden" in c.attrs && c.attrs.class === "tip-c");
  assert.strictEqual(c.kids[0].kids[0], "Asia/Tokyo");
});

test("the content of a zone is made once", () => {
  const p = setup("Asia/Tokyo");
  p.api.renderTimes(); p.api.renderTimes();
  assert.strictEqual(p.body.appended.length, 1);
});

test("zones that differ only in a sign get different contents", () => {
  const p = setup("Etc/GMT+1"), q = p.api;
  const n1 = { dataset: {}, setAttribute() {} }, n2 = { dataset: {}, setAttribute() {} };
  q.zoneTip(n1, "Etc/GMT+1"); q.zoneTip(n2, "Etc/GMT-1");
  assert.notStrictEqual(n1.dataset.tip, n2.dataset.tip);
});

test("the settings sample names its zone the same way", () => {
  const p = setup("Asia/Tokyo"), n = { dataset: {}, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; } };
  p.api.zoneTip(n, "Europe/Paris");
  assert.ok(n.dataset.tip && !("tabindex" in n.attrs) && !("title" in n.attrs));
  assert.strictEqual(p.body.appended[0].kids[0].kids[0], "Europe/Paris");
});

test("the zone is written once in the time column header, as plain text", () => {
  const p = setup("Asia/Tokyo");
  p.api.renderTimes();
  assert.strictEqual(p.h.textContent, "(Asia/Tokyo)");
});

test("a zone the browser does not know falls back to the browser's, in the header too", () => {
  const p = setup("Not/AZone");
  p.api.renderTimes();
  assert.strictEqual(p.h.textContent, "(" + Intl.DateTimeFormat().resolvedOptions().timeZone + ")");
});
