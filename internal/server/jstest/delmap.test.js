// Behaviour test of the "del-map" action in web/app.js (run by TestDeleteMappingScript with `node --test`): the confirmation
// of deleting a mapping names the folder and the local path of the row, and adds the locks sentence only when the row holds
// exclusives. The handler and the rt() helper are cut out of app.js and run against a small fake of the DOM.
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const src = fs.readFileSync(path.join(__dirname, "..", "web", "app.js"), "utf8");
const r0 = src.indexOf("const rt = ");
const r1 = src.indexOf("function el(", r0);
const h0 = src.indexOf('"del-map": (c) => {');
const h1 = src.indexOf("    revoke:", h0);
assert.ok(r0 > 0 && r1 > r0 && h0 > 0 && h1 > h0, "rt or the del-map handler was not found in app.js");

function run(lang, row) {
  const I18N = {};
  const all = JSON.parse(fs.readFileSync(path.join(__dirname, "..", "i18n", lang + ".json"), "utf8"));
  for (const k in all) if (k.startsWith("js.")) I18N[k] = all[k];
  const form = { dataset: {}, elements: { folder: {} }, submitted: 0, requestSubmit() { this.submitted++; } };
  const $ = (sel) => { assert.strictEqual(sel, "#f-del-map"); return form; };
  const make = new Function("I18N", "$", src.slice(r0, r1) + "\nreturn {" + src.slice(h0, h1).replace(/,\s*$/, "") + "};");
  make(I18N, $)["del-map"]({ row: { dataset: row } });
  return form;
}

for (const lang of ["en", "zh-CN", "zh-TW", "ja", "ko"]) {
  test(lang + ": the confirmation names the folder and the path", () => {
    const f = run(lang, { folder: "7", name: "Docs", path: "/home/a/docs" });
    assert.ok(f.dataset.confirm.includes("Docs"), f.dataset.confirm);
    assert.ok(f.dataset.confirm.includes("/home/a/docs"), f.dataset.confirm);
    assert.ok(!/[{}]/.test(f.dataset.confirm), "an unreplaced placeholder: " + f.dataset.confirm);
    assert.strictEqual(f.elements.folder.value, "7");
    assert.strictEqual(f.submitted, 1);
  });
}

test("a row with exclusives adds the locks sentence", () => {
  const f = run("en", { folder: "7", name: "Docs", path: "D:\\Docs", locks: "3" });
  assert.ok(f.dataset.confirm.includes("D:\\Docs"));
  assert.ok(f.dataset.confirm.includes("3 exclusives are released too"), f.dataset.confirm);
});
