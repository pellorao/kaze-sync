// Behaviour test of rt() in web/app.js (run by TestRtPlaceholdersReplacedOnce): placeholders are replaced in one pass, so a
// value that itself contains "{path}" is kept literally; the ".1" variant is used only for the count 1.
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const src = fs.readFileSync(path.join(__dirname, "..", "web", "app.js"), "utf8");
const r0 = src.indexOf("const rt = ");
const r1 = src.indexOf("function el(", r0);
assert.ok(r0 > 0 && r1 > r0, "rt was not found in app.js");
const rtOf = (I18N) => new Function("I18N", src.slice(r0, r1) + "\nreturn rt;")(I18N);

test("a value containing a placeholder name is not substituted again", () => {
  const rt = rtOf({ k: "{name} on {path}" });
  assert.strictEqual(rt("k", { name: "a{path}b", path: "/x" }), "a{path}b on /x");
});

test("a value containing $& or $1 stays literal", () => {
  const rt = rtOf({ k: "{name}!" });
  assert.strictEqual(rt("k", { name: "$&$1" }), "$&$1!");
});

test("the .1 variant applies at count 1 only", () => {
  const rt = rtOf({ k: "{n} files", "k.1": "{n} file" });
  assert.strictEqual(rt("k", { n: 1 }), "1 file");
  assert.strictEqual(rt("k", { n: 2 }), "2 files");
});
