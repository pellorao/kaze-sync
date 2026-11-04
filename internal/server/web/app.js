// The Web UI's script (design 4.1): dialogs and confirmations, context menus, time zones, live updates, the
// restore preview and the rule editors. No framework, no inline script: the pages carry data-* attributes.
"use strict";
(function () {
  const $ = (s, r) => (r || document).querySelector(s);
  const $$ = (s, r) => Array.from((r || document).querySelectorAll(s));
  const body = document.body;
  const main = $("main");
  const NS = "http://www.w3.org/2000/svg";

  function el(tag, attrs, ...kids) {
    const e = document.createElement(tag);
    for (const k in attrs || {}) e.setAttribute(k, attrs[k]);
    e.append(...kids);
    return e;
  }
  function icon(name) {
    const s = document.createElementNS(NS, "svg");
    s.setAttribute("class", "i");
    s.setAttribute("aria-hidden", "true");
    s.setAttribute("viewBox", "0 0 24 24");
    const u = document.createElementNS(NS, "use");
    u.setAttribute("href", "/static/icons.svg#i-" + name);
    s.append(u);
    return s;
  }
  function debounce(fn, ms) {
    let t;
    return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
  }

  // ---- time: the server writes UTC; the zone of the settings (or the browser's) is applied here (design 4.7)
  const browserTZ = Intl.DateTimeFormat().resolvedOptions().timeZone;
  function zoneName() {
    const z = body.dataset.tz;
    if (z) { try { new Intl.DateTimeFormat("en", { timeZone: z }); return z; } catch (e) { /* unknown here: use the browser's */ } }
    return browserTZ;
  }
  const formatters = {};
  function parts(ms, zone) {
    const f = formatters[zone] || (formatters[zone] = new Intl.DateTimeFormat("en-GB", { timeZone: zone, hourCycle: "h23",
      year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" }));
    const o = {};
    for (const p of f.formatToParts(new Date(ms))) o[p.type] = p.value;
    return o;
  }
  function fmt(ms, zone) {
    const p = parts(ms, zone || zoneName());
    return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}`;
  }
  function renderTimes(root) {
    const zone = zoneName();
    $$("time[datetime]", root).forEach((t) => {
      const d = Date.parse(t.getAttribute("datetime"));
      if (!isNaN(d)) { t.textContent = fmt(d, zone); t.title = zone; }
    });
  }
  // wall: the clock of the zone at instant ms, read as if it were UTC
  const wall = (ms, zone) => { const p = parts(ms, zone); return Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour, +p.minute, +p.second); };
  // parseLocal reads "2026-11-03 22:48[:27]" in the zone; a time that happens twice (clocks set back) or not at all
  // (set forward) takes the earlier reading. null when the text is not a time.
  function parseLocal(text, zone) {
    const m = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2})(?::(\d{2}))?$/.exec(text.trim());
    if (!m) return null;
    const guess = Date.UTC(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +(m[6] || 0));
    if (new Date(guess).getUTCMonth() !== +m[2] - 1) return null;
    const cands = [-864e5, 864e5].map((d) => guess - (wall(guess + d, zone) - (guess + d))).sort((a, b) => a - b);
    return cands.find((t) => wall(t, zone) === guess) ?? cands[0];
  }
  const utc = (ms) => new Date(ms).toISOString().replace(/\.\d+Z$/, "Z");

  // ---- dialogs and confirmations
  function openDialog(d) {
    if (d && !d.open) d.showModal();
  }
  document.addEventListener("click", (e) => {
    const t = e.target.closest("[data-dialog]");
    if (t) { openDialog($("#" + t.dataset.dialog)); return; }
    const c = e.target.closest("[data-close]");
    if (c) c.closest("dialog").close();
  });
  document.addEventListener("close", (e) => { // a dialog the URL opened takes its parameters with it, so reload does not reopen it
    const names = e.target.dataset && e.target.dataset.clear;
    if (!names) return;
    const u = new URL(location.href);
    names.split(",").forEach((n) => u.searchParams.delete(n));
    history.replaceState(null, "", u);
  }, true);

  const cf = $("#dlg-confirm");
  let pending = null;
  document.addEventListener("submit", (e) => {
    const f = e.target;
    if (!f.dataset || !f.dataset.confirm || f.dataset.confirmed) return;
    e.preventDefault();
    pending = { form: f, submitter: e.submitter };
    $("#cf-msg").textContent = f.dataset.confirm;
    const ok = $("#cf-ok");
    ok.textContent = f.dataset.confirmOk || "确定";
    ok.className = "btn " + ("danger" in f.dataset ? "danger" : "primary");
    cf.showModal();
  });
  if (cf) $("#cf-ok").addEventListener("click", () => {
    const p = pending;
    cf.close();
    if (!p) return;
    p.form.dataset.confirmed = "1";
    p.form.requestSubmit(p.submitter || undefined);
    delete p.form.dataset.confirmed;
  });

  // ---- context menus
  const menu = el("div", { id: "ctx", popover: "", class: "pop menu", role: "menu" });
  body.append(menu);
  let opener = null, target = null;
  const pageDir = () => new URLSearchParams(location.search).get("dir") || "";
  function goto(params) {
    const u = new URL(location.href);
    for (const k in params) { if (params[k] === null) u.searchParams.delete(k); else u.searchParams.set(k, params[k]); }
    location.href = u;
  }
  const restoreLabel = (d) => d.path ? "恢复此目录到某一时刻…" : "恢复整个文件夹到某一时刻…";
  const MENUS = {
    dir: (c) => [["打开", "open", "open"], ["恢复到某一时刻…", "restore", "restore-at"]],
    file: () => [["版本历史…", "history", "versions"], ["恢复到某一时刻…", "restore", "restore-at"]],
    gone: () => [["恢复最后的内容…", "restore", "restore-last"], ["版本历史…", "history", "versions"]],
    conflict: (c) => [["版本历史…", "history", "versions"], c.row.dataset.orig ? ["原文件的版本历史…", "history", "versions-orig"] : null, ["恢复到某一时刻…", "restore", "restore-at"]],
    unsynced: () => [["详情", "info", "detail"]],
    blank: (c) => [[restoreLabel(c.row.dataset), "restore", "restore-dir"]],
    "folder-head": (c) => {
      const n = +c.row.dataset.maps;
      return [[restoreLabel(c.row.dataset), "restore", "restore-dir"],
        ["删除文件夹…", "trash", "del-folder", { danger: true, disabled: n > 0, hint: n > 0 ? `仍有 ${n} 台设备映射, 先在设备页删除映射` : "" }]];
    },
    "map-row": () => [["编辑映射…", "edit", "edit-map"], ["删除映射…", "trash", "del-map", { danger: true }]],
    "dev-head": () => [["吊销设备…", "trash", "revoke", { danger: true }]],
  };
  const ACTIONS = {
    open: (c) => { location.href = $("a.nm", c.row).href; },
    versions: (c) => goto({ file: c.row.dataset.path }),
    "versions-orig": (c) => goto({ file: c.row.dataset.orig }),
    "restore-at": (c) => openRestore(c.row.dataset.path, c.row.dataset.kind === "dir"),
    "restore-dir": (c) => openRestore(c.row.dataset.path || "", !!c.row.dataset.path),
    "restore-last": (c) => { const f = $("#f-restore-last"); f.elements.path.value = c.row.dataset.path; f.requestSubmit(); },
    detail: (c) => {
      const d = $("#dlg-unsynced"), r = c.row.dataset;
      $('[data-fill="path"]', d).textContent = r.path;
      $('[data-fill="msg"]', d).textContent = r.msg;
      $('[data-fill="device"]', d).textContent = r.device;
      $('[data-fill="advice"]', d).textContent = r.advice;
      const t = $('[data-fill="since"]', d);
      t.setAttribute("datetime", new Date(+r.since * 1000).toISOString());
      renderTimes(d);
      openDialog(d);
    },
    "edit-map": (c) => {
      const d = $("#dlg-map-edit"), r = c.row.dataset, f = $("form", d);
      f.elements.folder.value = r.folder;
      f.elements.path.value = r.path;
      f.elements.adopt.checked = !!r.adopt;
      $('[data-fill="name"]', d).textContent = r.name;
      openDialog(d);
    },
    "del-map": (c) => { const f = $("#f-del-map"); f.elements.folder.value = c.row.dataset.folder; f.requestSubmit(); },
    revoke: () => $("#f-revoke").requestSubmit(),
    "del-folder": () => $("#f-del-folder").requestSubmit(),
  };

  // openMenu shows the menu of a row (or of a button or an empty area) at a point of the viewport.
  function openMenu(row, kind, x, y, from) {
    const items = (MENUS[kind] || (() => []))({ row }).filter(Boolean);
    if (!items.length) return;
    menu.replaceChildren();
    if (row.dataset.path && /^(dir|file|gone|conflict|unsynced)$/.test(kind)) menu.append(el("div", { class: "mh" }, row.dataset.path + (kind === "dir" ? "/" : "")));
    for (const [label, ic, act, o] of items) {
      const b = el("button", { type: "button", class: "mi" + (o && o.danger ? " danger" : ""), role: "menuitem" }, icon(ic), label);
      b.dataset.act = act;
      if (o && o.disabled) b.disabled = true;
      menu.append(b);
      if (o && o.hint) menu.append(el("div", { class: "hint" }, o.hint));
    }
    menu.setAttribute("aria-label", row.dataset.name || "操作");
    target = { row };
    opener = from || row;
    menu.showPopover();
    const w = menu.offsetWidth, h = menu.offsetHeight;
    menu.style.setProperty("left", Math.max(8, Math.min(x, innerWidth - w - 8)) + "px");
    menu.style.setProperty("top", (y + h > innerHeight - 8 ? Math.max(8, y - h) : y) + "px");
    const rowEl = row.closest("tr");
    if (rowEl) { rowEl.classList.add("menu-open"); const m = $(".more", rowEl); if (m) m.classList.add("on"); }
    const first = $(".mi:not(:disabled)", menu);
    if (first) first.focus({ focusVisible: false });
  }
  menu.addEventListener("toggle", (e) => {
    if (e.newState !== "closed") return;
    $$(".menu-open").forEach((r) => r.classList.remove("menu-open"));
    $$(".more.on").forEach((b) => b.classList.remove("on"));
    if (opener && opener.focus && !$("dialog[open]")) opener.focus({ focusVisible: false });
    flush();
  });
  menu.addEventListener("click", (e) => {
    const b = e.target.closest(".mi");
    if (!b || b.disabled) return;
    const c = target;
    opener = null; // the action takes the focus (a dialog), not the row
    menu.hidePopover();
    ACTIONS[b.dataset.act](c);
  });
  menu.addEventListener("keydown", (e) => {
    const items = $$(".mi:not(:disabled)", menu), i = items.indexOf(document.activeElement);
    if (e.key === "ArrowDown") items[(i + 1) % items.length].focus();
    else if (e.key === "ArrowUp") items[(i - 1 + items.length) % items.length].focus();
    else return;
    e.preventDefault();
  });
  addEventListener("scroll", () => { if (menu.matches(":popover-open")) menu.hidePopover(); }, true);

  let pressedAt = 0;
  document.addEventListener("contextmenu", (e) => {
    if (e.target.closest("dialog, .pop")) return;
    const row = e.target.closest("tr[data-kind]");
    const area = row ? null : e.target.closest("[data-menu-area]");
    if (!row && !area) return;
    e.preventDefault();
    if (Date.now() - pressedAt < 800) return; // the long press has opened it already
    if (row) openMenu(row, row.dataset.kind, e.clientX, e.clientY);
    else openMenu(area, area.dataset.menuArea, e.clientX, e.clientY);
  });
  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-menu]");
    if (!b) return;
    const row = b.closest("tr[data-path], tr[data-folder]") || b;
    const kind = b.dataset.menu === "row" ? row.dataset.kind : b.dataset.menu;
    const r = b.getBoundingClientRect();
    openMenu(row, kind, r.right - 200, r.bottom + 2, b);
  });
  document.addEventListener("keydown", (e) => { // Shift+F10 and the menu key open the menu of the row that has the focus
    if (e.key !== "ContextMenu" && !(e.shiftKey && e.key === "F10")) return;
    const row = e.target.closest("tr[data-kind]");
    if (!row) return;
    e.preventDefault();
    const r = $(".more", row).getBoundingClientRect();
    openMenu(row, row.dataset.kind, r.right - 200, r.bottom + 2, $("a", row) || row);
  });
  // long press (touch screens that send no contextmenu event): 500 ms, moving less than 10 px
  let press = null;
  document.addEventListener("touchstart", (e) => {
    const row = e.target.closest("tr[data-kind]"), t = e.touches[0];
    if (!row || e.touches.length > 1) return;
    cancelPress();
    const p = press = { x: t.clientX, y: t.clientY, row, ready: false };
    p.timer = setTimeout(() => { p.ready = true; }, 500);
  }, { passive: true });
  // lifting the finger after a long press must not click the row (a link) under it
  let swallow = false;
  document.addEventListener("click", (e) => { if (swallow) { swallow = false; e.preventDefault(); e.stopPropagation(); } }, true);
  const cancelPress = () => { if (press) clearTimeout(press.timer); press = null; };
  // the menu opens after the finger lifts: a popover opened under a finger that is still down is light-dismissed by its pointerup
  document.addEventListener("touchend", () => {
    const p = press;
    cancelPress();
    if (p && p.ready) { // the compatibility mouse events of this touch follow at once and would light-dismiss the menu, so it opens after them
      pressedAt = Date.now(); swallow = true;
      setTimeout(() => openMenu(p.row, p.row.dataset.kind, p.x, p.y + 24), 150);
    }
    setTimeout(() => { swallow = false; }, 600);
  });
  document.addEventListener("touchcancel", cancelPress);
  document.addEventListener("touchmove", (e) => {
    const t = e.touches[0];
    if (press && Math.hypot(t.clientX - press.x, t.clientY - press.y) > 10) cancelPress();
  }, { passive: true });

  // ---- notifications
  const notif = $("#notif");
  document.addEventListener("click", (e) => {
    const b = e.target.closest("[data-notif]");
    if (!b || !notif) return;
    notif.showPopover();
    const g = b.dataset.notif && document.getElementById(b.dataset.notif);
    if (g) g.scrollIntoView({ block: "start" });
  });
  if (notif) notif.addEventListener("toggle", (e) => { if (e.newState === "closed") flush(); });

  // ---- live updates: /events says that something changed; the marked regions of the page are fetched and replaced
  const liveURL = body.dataset.liveUrl || location.href;
  let es = null, lastGen = null, dirty = false;
  const busy = () => !!$("dialog[open]") || menu.matches(":popover-open");
  function flush() { if (dirty) refresh(); }
  async function refresh() {
    if (busy()) { dirty = true; return; }
    dirty = false;
    let r;
    try { r = await fetch(liveURL, { credentials: "same-origin", headers: main && main.dataset.folder ? { "X-Live": "1" } : {} }); } catch (e) { return; }
    if (r.redirected && new URL(r.url).pathname === "/login") { location.href = "/login"; return; }
    if (!r.ok || busy()) { dirty = true; return; }
    const doc = new DOMParser().parseFromString(await r.text(), "text/html");
    for (const cur of $$("[data-live]")) {
      const nu = $(`[data-live="${cur.dataset.live}"]`, doc);
      if (!nu) continue;
      if (cur === notif && notif.matches(":popover-open")) { dirty = true; continue; } // only the count changes while it is read
      cur.replaceChildren(...Array.from(nu.childNodes).map((n) => document.importNode(n, true)));
      renderTimes(cur);
    }
  }
  function connect() {
    if (es || document.hidden || !notif) return;
    es = new EventSource("/events");
    es.onopen = () => $("[data-conn]").classList.remove("on");
    es.onmessage = (e) => {
      const d = JSON.parse(e.data);
      if (main && main.dataset.folder && d.seq && String(d.seq[main.dataset.folder]) !== main.dataset.seq) $("[data-reload]").hidden = false;
      if (lastGen !== null && d.gen !== lastGen) refresh();
      lastGen = d.gen;
    };
    es.onerror = () => {
      $("[data-conn]").classList.add("on");
      if (es.readyState === EventSource.CLOSED) { // refused (the session has ended?): the refresh finds out; try again later
        es = null;
        refresh();
        setTimeout(connect, 10000);
      }
    };
  }
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) { if (es) es.close(); es = null; return; }
    connect();
    refresh();
  });
  document.addEventListener("click", (e) => { if (e.target.closest("[data-reload]")) location.href = liveURL; });

  // ---- restore to a point in time, with a preview
  const rd = $("#dlg-restore");
  let previewSeq = 0;
  function openRestore(path, isDir) {
    const f = rd.querySelector("form");
    f.elements.path.value = path;
    $('[data-fill="scope"]', rd).textContent = rd.dataset.folderName + (path ? " / " + path + (isDir ? "/" : "") : "");
    if (!f.elements.time.value) f.elements.time.value = fmt(Date.now() - 36e5);
    rd.showModal();
    previewNow();
  }
  function previewNow() {
    const f = rd.querySelector("form"), go = $("[data-restore-go]", rd), box = $("[data-preview]", rd);
    const ms = parseLocal(f.elements.time.value, zoneName());
    $("[data-oldwarn]", rd).hidden = ms === null || ms >= Date.now() - rd.dataset.keepDays * 864e5;
    go.disabled = true;
    if (ms === null) { box.hidden = true; $("span", go).textContent = "恢复"; return; }
    const seq = ++previewSeq;
    box.hidden = false;
    box.prepend(el("div", { class: "progress" }));
    const q = new URLSearchParams({ path: f.elements.path.value, time: utc(ms) });
    fetch(`${location.pathname}/restore-preview?${q}`, { credentials: "same-origin" })
      .then((r) => r.ok ? r.text() : Promise.reject(r.status))
      .then((html) => {
        if (seq !== previewSeq) return;
        const doc = new DOMParser().parseFromString(html, "text/html");
        box.replaceChildren(...Array.from(doc.body.childNodes).map((n) => document.importNode(n, true)));
        const n = +$("[data-count]", box).dataset.count;
        go.disabled = n === 0;
        $("span", go).textContent = n ? `恢复 ${n} 个文件` : "没有可恢复的文件";
      })
      .catch(() => { if (seq === previewSeq) box.hidden = true; });
  }
  if (rd) {
    const f = rd.querySelector("form");
    const zn = $("[data-tzname]", rd);
    zn.textContent = zoneName() + (body.dataset.tz ? ", 在设置中固定" : ", 跟随浏览器");
    f.elements.time.addEventListener("input", debounce(previewNow, 400));
    rd.addEventListener("click", (e) => {
      const c = e.target.closest("[data-ago]");
      if (!c) return;
      const zone = zoneName(), now = Date.now(), k = c.dataset.ago;
      let ms = now - (k === "1h" ? 36e5 : k === "1d" ? 864e5 : 7 * 864e5);
      if (k === "today") { const p = parts(now, zone); ms = parseLocal(`${p.year}-${p.month}-${p.day} 00:00:00`, zone); }
      f.elements.time.value = fmt(ms);
      previewNow();
    });
    f.addEventListener("submit", (e) => { // the server reads UTC: send the time the person typed as UTC
      const ms = parseLocal(f.elements.time.value, zoneName());
      if (ms === null) { e.preventDefault(); previewNow(); return; }
      f.elements.time.value = utc(ms);
    });
  }

  // ---- version history: the restore asks in its own row
  document.addEventListener("click", (e) => {
    const a = e.target.closest("[data-ask], [data-ask-cancel]");
    if (!a) return;
    const td = a.closest("td"), asking = "ask" in a.dataset;
    $("form.ask", td).hidden = !asking;
    a.closest("tr").classList.toggle("asking", asking);
  });

  // ---- exclusion-rule editors: a highlighted layer behind the textarea, errors from the server's own parser
  function ruleEditor(box) {
    const ta = $("textarea", box), pre = $("pre", box), list = box.nextElementSibling;
    const LH = 20, PAD = 6;
    let errs = new Map(), seq = 0;
    box.classList.add("js");
    function draw() {
      const cur = ta.value.slice(0, ta.selectionStart).split("\n").length;
      pre.replaceChildren(...ta.value.split("\n").map((t, i) =>
        el("span", { class: "l" + (t[0] === "#" ? " c" : "") + (errs.has(i + 1) ? " e" : "") + (errs.has(i + 1) && i + 1 === cur ? " cur" : "") }, t)));
      $$("[data-line]", list).forEach((d) => d.classList.toggle("cur", +d.dataset.line === cur));
      pre.scrollTop = ta.scrollTop;
      pre.scrollLeft = ta.scrollLeft;
    }
    const validate = debounce(async () => {
      const mine = ++seq;
      let out;
      try {
        const r = await fetch("/rules/validate", { method: "POST", body: ta.value, headers: { "Content-Type": "text/plain" }, credentials: "same-origin" });
        if (!r.ok) return;
        out = await r.json();
      } catch (e) { return; }
      if (mine !== seq) return;
      errs = new Map(out.errors.map((x) => [x.line, x.msg]));
      list.replaceChildren(...out.errors.map((x) => el("div", { class: "err-msg", "data-line": x.line }, `第 ${x.line} 行: ${x.msg}`)));
      ta.setAttribute("aria-invalid", errs.size ? "true" : "false");
      draw();
    }, 300);
    ta.addEventListener("input", () => { draw(); validate(); });
    ta.addEventListener("scroll", () => { pre.scrollTop = ta.scrollTop; pre.scrollLeft = ta.scrollLeft; });
    ["keyup", "click", "focus"].forEach((n) => ta.addEventListener(n, draw));
    ta.addEventListener("mousemove", (e) => {
      const line = Math.floor((e.clientY - ta.getBoundingClientRect().top + ta.scrollTop - PAD) / LH) + 1;
      ta.title = errs.has(line) ? `第 ${line} 行: ${errs.get(line)}` : "";
    });
    draw();
    if (ta.value) validate();
  }

  // ---- settings: the time zone card
  const tc = $("[data-tzcard]");
  if (tc) {
    const sel = $("select", tc), radios = $$("input[name=tz_mode]", tc), sample = $("[data-sample]", tc);
    const saved = tc.dataset.tz;
    $("[data-browser-tz]", tc).textContent = browserTZ;
    let zones = [];
    try { zones = Intl.supportedValuesOf("timeZone"); } catch (e) { /* an older browser: the saved zone and UTC */ }
    for (const z of new Set(["UTC", ...zones, saved || browserTZ])) if (!$(`option[value="${CSS.escape(z)}"]`, sel)) sel.append(el("option", { value: z }, z));
    sel.value = saved || browserTZ;
    const update = () => {
      const fixed = radios.find((r) => r.checked).value === "fixed";
      sel.disabled = !fixed;
      const zone = fixed ? sel.value : browserTZ;
      sample.textContent = fmt(Date.now(), zone);
      sample.title = zone;
    };
    radios.forEach((r) => r.addEventListener("change", update));
    sel.addEventListener("change", update);
    update();
  }

  // ---- small things: copy buttons, the enrollment countdown, the result bar
  document.addEventListener("click", async (e) => {
    const b = e.target.closest("[data-copy]");
    if (!b) return;
    const code = document.getElementById(b.dataset.copy);
    try { await navigator.clipboard.writeText(code.textContent); } catch (err) {
      const r = document.createRange();
      r.selectNodeContents(code);
      getSelection().removeAllRanges();
      getSelection().addRange(r);
      document.execCommand("copy");
    }
    const old = b.lastChild.textContent;
    b.lastChild.textContent = "已复制";
    setTimeout(() => { b.lastChild.textContent = old; }, 1500);
  });
  $$("[data-countdown]").forEach((c) => {
    const end = +c.dataset.countdown * 1000;
    const tick = () => {
      const s = Math.max(0, Math.round((end - Date.now()) / 1000));
      c.textContent = s ? `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}` : "已过期";
      if (!s) clearInterval(t);
    };
    const t = setInterval(tick, 1000);
    tick();
  });
  const toast = $("[data-toast]");
  if (toast) {
    const u = new URL(location.href);
    ["ok", "restored", "failed"].forEach((n) => u.searchParams.delete(n));
    if (u.href !== location.href) history.replaceState(null, "", u);
    setTimeout(() => toast.remove(), 5000);
    toast.addEventListener("click", (e) => { if (e.target.closest("[data-toast-close]")) toast.remove(); });
  }

  // ---- start
  renderTimes(document);
  $$("[data-ruleedit]").forEach(ruleEditor);
  $$("dialog[data-autoopen]").forEach(openDialog);
  const hit = decodeURIComponent(location.hash.slice(1));
  if (hit) {
    const row = $$("tr[data-path]").find((r) => r.dataset.path === hit);
    if (row) { row.classList.add("hit"); row.scrollIntoView({ block: "center" }); setTimeout(() => row.classList.remove("hit"), 2000); }
  }
  connect();
})();
