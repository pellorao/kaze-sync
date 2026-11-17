// The service worker of the Web Push notifications (design 4.3).
self.addEventListener("push", (e) => {
  let d = {};
  try { d = e.data.json(); } catch (_) { /* a push must always show something */ }
  e.waitUntil(self.registration.showNotification(d.title || "kaze-sync", {
    body: d.body || "", tag: d.tag || undefined, data: { url: d.url || "/" },
  }));
});
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));
self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || "/";
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((list) => {
    const c = list.find((w) => new URL(w.url).origin === self.location.origin);
    return c ? c.focus().then((w) => w.navigate(url)) : self.clients.openWindow(url);
  }).catch(() => self.clients.openWindow(url)));
});
