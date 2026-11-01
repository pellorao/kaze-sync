// Asks for confirmation before submitting a form that has a data-confirm attribute.
document.addEventListener("submit", function (e) {
  var msg = e.target.dataset && e.target.dataset.confirm;
  if (msg && !window.confirm(msg)) e.preventDefault();
});
