// Behaviour test of the rule editor's preview in web/app.js, run by TestHitsFilterScript with `node --test`: the
// preview controller is cut out of app.js and run against a small fake of the DOM and of fetch. The glob filter box
// above the list of hits must survive every refresh: same element, same value, same caret, same focus, so a character
// typed while an answer is on its way is never lost (a refresh used to rebuild the box from the answer).
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const src = fs.readFileSync(path.join(__dirname, "..", "web", "app.js"), "utf8");
const a = src.indexOf("const preview = (() => {");
const b = src.indexOf("go.addEventListener(\"click\"", a);
assert.ok(a > 0 && b > a, "the preview controller of app.js was not found");
const section = src.slice(a, b);

class N {
  constructor(attrs = {}) { Object.assign(this, { children: [], dataset: {}, hidden: false, value: "", selectionStart: 0, selectionEnd: 0, handlers: {} }, attrs); }
  get firstElementChild() { return this.children[0] || null; }
  replaceChildren(...n) { this.children = n; n.forEach((c) => { c.parent = this; }); }
  append(...n) { this.replaceChildren(...this.children, ...n); }
  replaceWith(n) { const p = this.parent; p.children = p.children.map((c) => (c === this ? n : c)); n.parent = p; this.parent = null; }
  find(sel) {
    for (const c of this.children) {
      if (sel === "[data-hits-filter]" ? c.isFilter : sel === "[data-hits]" ? "hits" in c.dataset : false) return c;
      const r = c.find(sel); if (r) return r;
    }
    return null;
  }
  addEventListener(t, f) { this.handlers[t] = f; }
  setAttribute(k, v) { (this.attrs || (this.attrs = {}))[k] = String(v); }
  getAttribute(k) { return (this.attrs || {})[k] ?? null; }
  focus() { N.doc.activeElement = this; }
  setSelectionRange(s, e) { this.selectionStart = s; this.selectionEnd = e; }
  matches(sel) { return sel === "[data-hits-filter]" && !!this.isFilter; }
}

function setup() {
  const doc = N.doc = { activeElement: null, createDocumentFragment: () => new N(), importNode: (n) => n };
  const hitsBox = new N(), go = new N(), ta = new N({ value: "*.log" });
  const form = { getAttribute: () => "/rules" };
  const pending = []; // requests not answered yet: { filter, answer(html) }
  const fetchFn = (url, o) => new Promise((res) => pending.push({ filter: o.body.get("filter"), answer: (ok = true) => res({ ok, text: async () => o.body.get("filter") }) }));
  class Parser { // the answer's text is the filter it was asked for; the page of the answer holds a box with it
    parseFromString(t) {
      const box = new N({ isFilter: false }), f = new N({ isFilter: true, value: t, hidden: true }), h = new N({ dataset: { hits: "3" } });
      box.append(h, f);
      return { body: { childNodes: [box] } };
    }
  }
  let timer = null;
  const debounce = (fn) => (...x) => { timer = () => fn(...x); };
  const $ = (sel, root) => root.find(sel);
  const el = () => new N();
  const rt = (k) => k;
  const run = new Function("document", "DOMParser", "fetch", "URLSearchParams", "debounce", "$", "el", "rt",
    "ta", "hitsBox", "go", "form", "saved0",
    section + "\nreturn { preview };");
  const { preview } = run(doc, Parser, fetchFn, URLSearchParams, debounce, $, el, rt, ta, hitsBox, go, form, "");
  const settle = () => new Promise((r) => setImmediate(r));
  const refilter = () => hitsBox.handlers.input({ target: hitsBox.find("[data-hits-filter]") }); // the page's own handler
  return { go, doc, hitsBox, pending, preview, refilter, flush: () => { const t = timer; timer = null; t(); }, settle };
}

async function shown(s) { // the first preview: the box comes from the answer, hidden as the page draws it
  const p = s.preview.ask(); s.pending.shift().answer(); await p; await s.settle();
  return s.hitsBox.find("[data-hits-filter]");
}

test("the first answer shows the filter box", async () => {
  const s = setup();
  const f = await shown(s);
  assert.ok(f && f.hidden === false);
});

test("an answer arriving while the person types does not touch the box", async () => {
  const s = setup();
  const f = await shown(s);
  f.value = "lo"; s.doc.activeElement = f; f.selectionStart = f.selectionEnd = 2;
  s.refilter(); s.flush(); // the pause: the question for "lo" goes out
  assert.strictEqual(s.pending.length, 1);
  f.value = "log"; f.selectionStart = f.selectionEnd = 3; s.refilter(); // typing goes on while it is on its way
  s.pending.shift().answer(); await s.settle();
  assert.strictEqual(s.hitsBox.find("[data-hits-filter]"), f, "the box is the same element");
  assert.strictEqual(f.value, "log", "no character is lost");
  s.flush(); // the newer question goes out and its answer replaces the list
  assert.strictEqual(s.pending[0].filter, "log");
  s.pending.shift().answer(); await s.settle();
  assert.strictEqual(s.hitsBox.find("[data-hits-filter]"), f);
  assert.strictEqual(f.value, "log");
  assert.strictEqual(s.doc.activeElement, f);
});

test("a refresh keeps the caret where it was", async () => {
  const s = setup();
  const f = await shown(s);
  f.value = "lg"; s.doc.activeElement = f; f.selectionStart = f.selectionEnd = 1;
  s.refilter(); s.flush();
  s.pending.shift().answer(); await s.settle();
  assert.strictEqual(s.hitsBox.find("[data-hits-filter]"), f);
  assert.deepStrictEqual([f.value, f.selectionStart, f.selectionEnd], ["lg", 1, 1]);
});

test("the button is aria-disabled, not disabled, while the question is on its way: it keeps the focus", async () => {
  const s = setup();
  s.go.focus();
  const p = s.preview.ask();
  assert.strictEqual(s.go.getAttribute("aria-disabled"), "true");
  assert.notStrictEqual(s.go.disabled, true, "a disabled button drops the focus to the body");
  assert.strictEqual(s.doc.activeElement, s.go);
  s.pending.shift().answer(); await p;
  assert.strictEqual(s.go.getAttribute("aria-disabled"), "false");
  assert.strictEqual(s.doc.activeElement, s.go);
});

test("the link to the settings in a rule dialog asks before it leaves with unsaved changes", () => {
  const a = src.indexOf("// the link to the settings is a third way out");
  const b = src.indexOf("dlg.addEventListener(\"cancel\"", a);
  assert.ok(a > 0 && b > a, "the link handler of app.js was not found");
  const run = (dirty) => {
    let handler = null, asked = null;
    const link = { href: "http://x/settings", addEventListener: (t, f) => { handler = f; } };
    const $$ = () => [link];
    const ask = (...x) => { asked = x; };
    const loc = {};
    new Function("$$", "dlg", "dirty", "ask", "rt", "location", src.slice(a, b))($$, {}, () => dirty, ask, (k) => k, loc);
    let prevented = false;
    handler({ defaultPrevented: false, button: 0, preventDefault() { prevented = true; } });
    return { prevented, asked, loc };
  };
  const d = run(true);
  assert.ok(d.prevented && d.asked, "dirty: the click is held and the question is put");
  assert.strictEqual(d.loc.href, undefined, "not gone yet");
  d.asked[2]();
  assert.strictEqual(d.loc.href, "http://x/settings", "yes goes on");
  const c = run(false);
  assert.ok(!c.prevented && !c.asked, "clean: the link just works");
});
