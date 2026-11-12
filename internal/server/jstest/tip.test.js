// Behaviour tests of the tip box in web/app.js (design 4.8), run by TestTipBoxScript with `node --test`; no dependency:
// the tip section of app.js is cut out and run against a small fake of the DOM and of the clock.
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const src = fs.readFileSync(path.join(__dirname, "..", "web", "app.js"), "utf8");
const a = src.indexOf("// ---- tips");
const b = src.indexOf("// ---- notifications");
assert.ok(a > 0 && b > a, "the tip section of app.js was not found");
const section = src.slice(a, b);

class Node {
  constructor(tag) { this.tag = tag; this.children = []; this.hidden = false; this.connected = true; this.dataset = {}; this.attrs = {}; this.style = { setProperty() {} }; this.rect = { left: 0, top: 0, right: 0, bottom: 0 }; this.offsetWidth = 100; this.offsetHeight = 40; this.classes = new Set(); this.parent = null; }
  setAttribute(k, v) { this.attrs[k] = v; }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  append(...k) { for (const n of k) { n.parent = this; this.children.push(n); } }
  replaceChildren(...k) { this.children = []; this.append(...k); }
  get childNodes() { return this.children; }
  cloneNode() { const c = new Node(this.tag); c.text = this.text; return c; }
  get isConnected() { return this.connected; }
  contains(n) { for (let x = n; x; x = x.parent) if (x === this) return true; return false; }
  matches(sel) { return sel === "[data-tip]" ? "tip" in this.dataset : sel === ":focus-visible" ? !!this.focusVisible : false; }
  closest(sel) { for (let x = this; x; x = x.parent) if (x.matches(sel)) return x; return null; }
  getBoundingClientRect() { return this.rect; }
  get classList() { return { contains: (c) => this.classes.has(c) }; }
}

// A page with n rows; row i has a trigger (the name link described by the content of its icon's tip).
function setup() {
  const listeners = {}, win = {}, timers = [];
  let now = 1000, clock = 0;
  const byId = {};
  const document = {
    addEventListener(t, f) { (listeners[t] = listeners[t] || []).push(f); },
    getElementById: (id) => byId[id] || null,
    documentElement: { clientWidth: 1000, offsetWidth: 1000, clientHeight: 800 }, // the visible viewport, without scrollbars
  };
  const body = new Node("body");
  const el = (tag) => new Node(tag);
  const setTimeout_ = (f, ms) => { timers.push({ f, at: clock + ms, id: timers.length + 1 }); return timers.length; };
  const clearTimeout_ = (id) => { const t = timers.find((x) => x.id === id); if (t) t.dead = true; };
  const run = new Function("el", "body", "document", "addEventListener", "performance", "setTimeout", "clearTimeout",
    section + "\nreturn { tip, state: () => tipFor, dropStaleTip, placeTip };");
  const api = run(el, body, document, (t, f) => { (win[t] = win[t] || []).push(f); }, { now: () => now }, setTimeout_, clearTimeout_);
  const fire = (t, e) => { for (const f of listeners[t] || []) f(e); };
  const scroll = () => { for (const f of win.scroll || []) f({}); };
  const tick = (ms) => { clock += ms; now += ms; for (const t of timers) if (!t.dead && t.at <= clock) { t.dead = true; t.f(); } };
  function row(i, top) {
    const icon = new Node("span"); icon.dataset.tip = "wt" + i; icon.rect = { left: 20, top, right: 40, bottom: top + 20 };
    const c = new Node("span"); c.classes.add("tip-c"); c.attrs.id = "wt" + i; const t = new Node("span"); t.text = "tip " + i; c.append(t); icon.append(c);
    byId["wt" + i] = c;
    const link = new Node("a"); link.attrs["aria-describedby"] = "wt" + i; link.focusVisible = true; link.rect = { left: 50, top, right: 200, bottom: top + 20 };
    return { icon, link, c };
  }
  const tipPos = () => { const pos = {}; api.tip.style = { setProperty(k, v) { pos[k] = parseFloat(v); } }; return pos; };
  return { api, tipPos, root: document.documentElement, fire, scroll, tick, row, document, setNow: (v) => { now = v; }, advance: (ms) => { now += ms; } };
}

test("hovering shows the tip, leaving both trigger and tip hides it after the delay", () => {
  const p = setup(), r = p.row(1, 100);
  p.fire("pointerover", { target: r.icon, pointerType: "mouse" });
  assert.strictEqual(p.api.tip.hidden, false);
  assert.strictEqual(p.api.tip.children[0].text, "tip 1");
  p.fire("pointerout", { target: r.icon, relatedTarget: null, pointerType: "mouse" });
  p.tick(200);
  assert.strictEqual(p.api.tip.hidden, false, "still within the 0.3 s delay");
  p.tick(200);
  assert.strictEqual(p.api.tip.hidden, true);
});

test("the pointer can move onto the tip", () => {
  const p = setup(), r = p.row(1, 100);
  p.fire("pointerover", { target: r.icon, pointerType: "mouse" });
  p.fire("pointerout", { target: r.icon, relatedTarget: p.api.tip, pointerType: "mouse" });
  p.tick(1000);
  assert.strictEqual(p.api.tip.hidden, false);
});

test("Escape hides the tip", () => {
  const p = setup(), r = p.row(1, 100);
  p.fire("pointerover", { target: r.icon, pointerType: "mouse" });
  p.fire("keydown", { key: "Escape" });
  assert.strictEqual(p.api.tip.hidden, true);
});

test("keyboard focus of the row's link shows the tip of its icon; leaving hides it", () => {
  const p = setup(), r = p.row(2, 100);
  p.fire("focusin", { target: r.link });
  assert.strictEqual(p.api.tip.hidden, false);
  assert.strictEqual(p.api.state(), r.icon);
  p.fire("focusout", { target: r.link });
  assert.strictEqual(p.api.tip.hidden, true);
});

test("a scroll the browser makes to reveal the focused element keeps the tip (finding 1)", () => {
  const p = setup(), r = p.row(3, 700);
  p.fire("focusin", { target: r.link });
  p.advance(30);
  r.icon.rect = { left: 20, top: 300, right: 40, bottom: 320 }; // the page scrolled
  p.scroll();
  assert.strictEqual(p.api.tip.hidden, false, "tip closed by the focus scroll");
  assert.strictEqual(p.api.state(), r.icon);
});

test("a later scroll, and any scroll of a hovered tip, hides it", () => {
  const p = setup(), r = p.row(3, 100);
  p.fire("focusin", { target: r.link });
  p.advance(2000);
  p.scroll();
  assert.strictEqual(p.api.tip.hidden, true);
  p.fire("pointerover", { target: r.icon, pointerType: "mouse" });
  p.scroll();
  assert.strictEqual(p.api.tip.hidden, true);
});

test("a tap shows the tip, a second tap or a tap elsewhere hides it", () => {
  const p = setup(), r = p.row(1, 100), other = new Node("div");
  p.fire("click", { target: r.icon, pointerType: "touch" });
  assert.strictEqual(p.api.tip.hidden, false);
  p.fire("click", { target: r.icon, pointerType: "touch" });
  assert.strictEqual(p.api.tip.hidden, true);
  p.fire("click", { target: r.icon, pointerType: "touch" });
  p.fire("click", { target: other, pointerType: "touch" });
  assert.strictEqual(p.api.tip.hidden, true);
});

test("a live update that removed the trigger takes the tip away (finding 2)", () => {
  const p = setup(), r = p.row(1, 100);
  p.fire("focusin", { target: r.link });
  p.api.dropStaleTip();
  assert.strictEqual(p.api.tip.hidden, false, "trigger still on the page");
  r.icon.connected = false; // replaceChildren of the data-live region
  p.api.dropStaleTip();
  assert.strictEqual(p.api.tip.hidden, true);
  assert.strictEqual(p.api.state(), null);
});

test("refresh calls dropStaleTip after it replaced the live regions", () => {
  assert.match(src, /renderTimes\(cur\);\s*\}\s*dropStaleTip\(\);/);
});

test("the tip is clamped to the visible viewport, which excludes the scrollbar", () => {
  const p = setup(), r = p.row(1, 100), pos = p.tipPos();
  p.root.clientWidth = 985; // 15 px vertical scrollbar
  r.icon.rect = { left: 960, top: 100, right: 980, bottom: 120 };
  p.fire("pointerover", { target: r.icon, pointerType: "mouse" });
  assert.strictEqual(pos.left, 985 - 100 - 8);
  r.icon.rect = { left: 0, top: 770, right: 20, bottom: 790 };
  p.root.clientHeight = 785;
  p.api.placeTip(r.icon);
  assert.ok(pos.top + 40 <= 785 - 8, "above the trigger when below does not fit");
});

test("no popup placement reads innerWidth/innerHeight (they include the scrollbar)", () => {
  assert.doesNotMatch(src, /\binner(Width|Height)\b/);
});

test("a stable scrollbar gutter on a page too short to scroll narrows the viewport (offsetWidth, not clientWidth)", () => {
  const p = setup(), r = p.row(1, 100), pos = p.tipPos();
  p.root.offsetWidth = 985; // clientWidth stays 1000: no scrollbar, but 15 px stay reserved
  r.icon.rect = { left: 960, top: 100, right: 980, bottom: 120 };
  p.fire("pointerover", { target: r.icon, pointerType: "mouse" });
  assert.strictEqual(pos.left, 985 - 100 - 8);
});
