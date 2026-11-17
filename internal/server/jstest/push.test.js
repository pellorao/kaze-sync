// Behaviour test of the push notifications card in web/app.js (run by TestPushCardScript with `node --test`): which
// problem the card names first, the id it gives this browser's subscription (the server's subID, passed in by the Go
// test), and the decoding of the server key. The helpers are cut out of app.js and run without a DOM.
"use strict";
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const src = fs.readFileSync(path.join(__dirname, "..", "web", "app.js"), "utf8");
const a = src.indexOf("  function pushProblem(s) {");
const b = src.indexOf("  function pushCard(pc) {", a);
assert.ok(a > 0 && b > a, "the push helpers were not found in app.js");
const { pushProblem, pushSubID, b64u, sameBytes } = new Function(src.slice(a, b) + "\nreturn { pushProblem, pushSubID, b64u, sameBytes };")();

const fine = { err: "", available: true, secure: true, sw: true, push: true, perm: "default" };
const dicts = ["en", "zh-CN", "zh-TW", "ja", "ko"].map((l) => JSON.parse(fs.readFileSync(path.join(__dirname, "..", "i18n", l + ".json"), "utf8")));

test("nothing stops push when all is well, whatever the permission short of denied", () => {
  for (const perm of ["default", "granted"]) assert.strictEqual(pushProblem({ ...fine, perm }), "");
});

test("the first thing that stops push is named", () => {
  const cases = [
    [{ err: "login" }, "js.push.login"],
    [{ err: "network" }, "js.push.network"],
    [{ err: "no_vapid_key" }, "js.push.server"],
    [{ err: "500", available: false }, "js.push.server"], // the list failed: nothing else is known
    [{ available: false, secure: false }, "js.push.selfsigned"], // the server's fault comes before this device's
    [{ secure: false }, "js.push.insecure"],
    [{ sw: false }, "js.push.insecure"], // browsers hide the service worker API outside a secure context
    [{ push: false, perm: "denied" }, "js.push.unsupported"],
    [{ perm: "denied" }, "js.push.denied"],
  ];
  for (const [s, want] of cases) {
    const key = pushProblem({ ...fine, ...s });
    assert.strictEqual(key, want, JSON.stringify(s));
    for (const d of dicts) assert.ok(d[key], key + " is missing from a dictionary");
  }
});

test("this browser's id is the server's id of its endpoint", async () => {
  const ep = process.env.KZ_PUSH_ENDPOINT || "https://fcm.googleapis.com/fcm/send/abc:DEF-123";
  const id = await pushSubID(ep);
  assert.match(id, /^[0-9a-f]{16}$/);
  if (process.env.KZ_PUSH_ID) assert.strictEqual(id, process.env.KZ_PUSH_ID);
});

test("the server key decodes from unpadded base64url and compares by bytes", () => {
  const raw = Uint8Array.from({ length: 65 }, (_, i) => (i * 37 + 251) & 255); // has bytes that encode to - and _
  const s = Buffer.from(raw).toString("base64url");
  assert.ok(!s.includes("=") && /[-_]/.test(s), s);
  const k = b64u(s);
  assert.deepStrictEqual(Array.from(k), Array.from(raw));
  for (const n of [1, 2, 3]) assert.deepStrictEqual(Array.from(b64u(Buffer.from(raw.slice(0, n)).toString("base64url"))), Array.from(raw.slice(0, n)));
  assert.ok(sameBytes(raw.buffer, k));
  assert.ok(!sameBytes(raw.slice(1).buffer, k));
  assert.ok(!sameBytes(null, k)); // a subscription made without a key
  const other = raw.slice();
  other[64] ^= 1;
  assert.ok(!sameBytes(other.buffer, k));
});
