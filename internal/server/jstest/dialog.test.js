// Behaviour tests of openDialog in web/app.js, run by TestDialogScript with `node --test`; no dependency: the dialog
// section of app.js is cut out and run against a small fake of the DOM.
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const src = fs.readFileSync(path.join(__dirname, "..", "web", "app.js"), "utf8");
const a = src.indexOf("// ---- dialogs and confirmations");
const b = src.indexOf('document.addEventListener("click"', a);
assert.ok(a > 0 && b > a, "the dialog section of app.js was not found");
const section = src.slice(a, b);

// A dialog holding controls; showModal() focuses the first one, as the browser does (the first focusable element).
function setup(controls) {
  const doc = { activeElement: null };
  const d = {
    open: false,
    showModal() { this.open = true; doc.activeElement = controls[0]; },
    querySelector(sel) { // only the one selector openDialog uses: the checked radio of a group
      const m = /^input\[type=radio\]\[name="([^"]+)"\]:checked$/.exec(sel);
      assert.ok(m, "unexpected selector " + sel);
      return controls.find((c) => c.type === "radio" && c.name === m[1] && c.checked) || null;
    },
  };
  for (const c of controls) c.focus = () => { doc.activeElement = c; };
  const run = new Function("document", "location", section + "\nreturn { openDialog, returnFocus };");
  return { d, doc, openDialog: run(doc, { href: "http://x/folders/1" }).openDialog };
}
const radio = (name, value, checked) => ({ type: "radio", name, value, checked: !!checked });

test("the checked radio takes the focus, not the first one of its group", () => {
  const cs = [radio("mode", "free"), radio("mode", "single", true), radio("mode", "exclusive")];
  const { d, doc, openDialog } = setup(cs);
  openDialog(d);
  assert.ok(d.open);
  assert.strictEqual(doc.activeElement, cs[1]);
});

test("a dialog whose first control is not a radio keeps the browser's choice", () => {
  const cs = [{ type: "text", name: "path" }, radio("mode", "free"), radio("mode", "single", true)];
  const { d, doc, openDialog } = setup(cs);
  openDialog(d);
  assert.strictEqual(doc.activeElement, cs[0]);
});

test("a group with nothing checked keeps the browser's choice", () => {
  const cs = [radio("mode", "free"), radio("mode", "single")];
  const { d, doc, openDialog } = setup(cs);
  openDialog(d);
  assert.strictEqual(doc.activeElement, cs[0]);
});

test("an open dialog is left alone", () => {
  const cs = [radio("mode", "free"), radio("mode", "single", true)];
  const { d, doc, openDialog } = setup(cs);
  d.open = true;
  openDialog(d);
  assert.strictEqual(doc.activeElement, null);
});

// returnFocus: a dialog the URL opened has no opener, so the focus goes to the link that leads to it (surplusfix1117 K3).
function page(links, active) {
  const doc = {
    activeElement: active,
    body: { tag: "body" },
    querySelectorAll(sel) { assert.strictEqual(sel, "a[href]"); return links; },
  };
  for (const l of links) l.focus = () => { doc.activeElement = l; };
  const run = new Function("document", "location", section + "\nreturn returnFocus;");
  return { doc, returnFocus: run(doc, { href: "http://x/folders/1?dir=d&file=d%2Fa.txt" }) };
}
const link = (href, shown = true) => ({ href: "http://x" + href, getClientRects: () => (shown ? [1] : []) });
const dlg = (inside) => ({ contains: (n) => n === inside });

test("the focus goes to the visible link of the same file when the dialog closes with the focus inside it", () => {
  const x = { tag: "x" };
  const links = [link("/folders/1?dir=d&file=d%2Fb.txt"), link("/folders/1?dir=d&file=d%2Fa.txt", false), link("/folders/1?dir=d&file=d%2Fa.txt")];
  const { doc, returnFocus } = page(links, x);
  returnFocus(dlg(x), [["file", "d/a.txt"]]);
  assert.strictEqual(doc.activeElement, links[2]);
});

test("the focus lost to the body goes to the link as well", () => {
  const links = [link("/folders/1?dir=d&held=2")];
  const { doc, returnFocus } = page(links, null);
  doc.activeElement = doc.body;
  returnFocus(dlg(null), [["held", "2"]]);
  assert.strictEqual(doc.activeElement, links[0]);
});

test("a focus the browser already gave back is kept", () => {
  const opener = { tag: "opener" }, links = [link("/folders/1?held=2")];
  const { doc, returnFocus } = page(links, opener);
  returnFocus(dlg({}), [["held", "2"]]);
  assert.strictEqual(doc.activeElement, opener);
});

test("without a link for the parameter the focus stays where it is", () => {
  const x = { tag: "x" }, links = [link("/folders/1?held=3")];
  const { doc, returnFocus } = page(links, x);
  returnFocus(dlg(x), [["held", "2"], ["rv", "9"]]);
  assert.strictEqual(doc.activeElement, x);
});
