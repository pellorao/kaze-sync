// Keyboard focus around the notification panel (notif-focus K1, K2), run by TestNotifFocusScript with `node --test`:
// two sections of web/app.js are cut out and run against a small fake of the DOM.
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const src = fs.readFileSync(path.join(__dirname, "..", "web", "app.js"), "utf8");
function cut(from, to) {
  const a = src.indexOf(from), b = src.indexOf(to, a);
  assert.ok(a > 0 && b > a, "section not found: " + from);
  return src.slice(a, b);
}

// K1: ask() runs no() when the confirmation closes without its button (Cancel, Esc), not after the button.
const confirmSection = cut('const cf = $("#dlg-confirm");', "// ---- context menus");
function confirm() {
  const handlers = {}, ok = { textContent: "", className: "", addEventListener: (n, f) => { handlers.ok = f; } };
  const cf = {
    open: false,
    showModal() { this.open = true; },
    close() { this.open = false; this.fire(); },
    fire() { handlers.close(); }, // the browser fires "close" after the dialog closed
    addEventListener: (n, f) => { handlers[n] = f; },
  };
  const els = { "#dlg-confirm": cf, "#cf-ok": ok, "#cf-msg": {} };
  const doc = { addEventListener() {} };
  const run = new Function("$", "document", "rt", confirmSection + "\nreturn ask;");
  return { cf, ok, handlers, ask: run((s) => els[s], doc, (k) => k) };
}

test("cancelling a question runs its no() once", () => {
  const { cf, ask } = confirm();
  let yes = 0, no = 0;
  ask("m", "ok", () => yes++, () => no++);
  cf.close();
  assert.deepStrictEqual([yes, no], [0, 1]);
});

test("answering with the button runs yes() and not no()", () => {
  const { handlers, ask } = confirm();
  let yes = 0, no = 0;
  ask("m", "ok", () => yes++, () => no++);
  handlers.ok();
  assert.deepStrictEqual([yes, no], [1, 0]);
});

test("a question asked without no() closes quietly", () => {
  const { cf, ask } = confirm();
  ask("m", "ok", () => {});
  cf.close();
});

// K2: a button that opens the panel is its invoker (source), so Tab goes from the button into the panel.
const notifSection = cut('document.addEventListener("click", (e) => {\n    const b = e.target.closest("[data-notif]");', "if (notif) notif.addEventListener");
test("the button that opens the panel is passed as its source", () => {
  let click, shown = null;
  const notif = { showPopover(o) { shown = o; } };
  const btn = { dataset: { notif: "" } };
  const doc = { addEventListener: (n, f) => { click = f; }, getElementById: () => null };
  new Function("document", "notif", notifSection)(doc, notif);
  click({ target: { closest: (s) => { assert.strictEqual(s, "[data-notif]"); return btn; } } });
  assert.ok(shown && shown.source === btn, "showPopover got no source");
});
