"use strict";
// Only settings forms participate; searches and operation buttons are not edits.
const changedForms = new Set();
function formChanged(event) {
  const form = event.target.closest("form[data-config-form]");
  if (form) {
    changedForms.add(form);
    const label = form.querySelector("[data-admin-unsaved-label]");
    if (label) label.textContent = "Unsaved changes";
  }
}
document.addEventListener("input", formChanged);
document.addEventListener("change", formChanged);
document.addEventListener("submit", event => { if (!event.defaultPrevented) changedForms.delete(event.target); });
window.addEventListener("beforeunload", event => {
  if (Array.from(changedForms).some(form => form.isConnected)) { event.preventDefault(); event.returnValue = ""; }
});
