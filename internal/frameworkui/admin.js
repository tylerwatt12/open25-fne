"use strict";
// Presentation only. No configuration polling, preflight, or radio actions.
if (document.body.classList.contains("administration")) {
  const root = document.querySelector(".ts-admin"), themeKey = "trunksystem-admin-theme";
  function theme(value) {
    root.style.colorScheme = value;
    document.body.dataset.theme = value;
    for (const button of document.querySelectorAll("[data-admin-theme]")) button.textContent = value === "dark" ? "Light" : "Dark";
  }
  let preference;
  try { preference = localStorage.getItem(themeKey); } catch (_) {}
  theme(preference === "light" || preference === "dark" ? preference : matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  for (const form of document.querySelectorAll("form[data-admin-unsaved]")) {
    changedForms.add(form);
    const label = form.querySelector("[data-admin-unsaved-label]"); if (label) label.textContent = "Unsaved changes";
  }
  // Retain only a navigation destination until the server confirms a successful Save.
  const navigationKey = "open25-admin-after-save";
  try {
    const pending = JSON.parse(sessionStorage.getItem(navigationKey));
    sessionStorage.removeItem(navigationKey);
    if (pending && new URL(location.href).searchParams.has("saved") && pending.savedPath === location.pathname) location.assign(pending.destination);
  } catch (_) {}
  let pendingDestination;
  function navigate(destination) {
    const form = [...changedForms].find(form => form.isConnected);
    if (!form) { location.assign(destination); return; }
    pendingDestination = destination;
    let notice = document.querySelector("[data-unsaved-navigation]");
    if (!notice) {
      notice = document.createElement("div"); notice.className = "ts-dirty"; notice.dataset.unsavedNavigation = ""; notice.setAttribute("role", "alert");
      const text = document.createElement("span"); text.textContent = "You have unsaved changes."; notice.append(text);
      for (const [action, label] of [["save", "Save and continue"], ["discard", "Discard and continue"], ["stay", "Stay here"]]) {
        const button = document.createElement("button"); button.type = "button"; button.dataset.unsavedAction = action; button.textContent = label; notice.append(button);
      }
      document.querySelector(".ts-main").prepend(notice);
    }
    notice.querySelector("button").focus(); notice.scrollIntoView({block:"nearest"});
  }
  function numberPair(input) {
    const pair = input.closest(".ts-numberpair"); if (!pair) return;
    const decimal = pair.querySelector("[data-number-decimal]"), hex = pair.querySelector("[data-number-hex]"), source = pair.querySelector("[data-number-source]");
    if (input !== decimal && input !== hex) return;
    const fromHex = input === hex; source.value = fromHex ? "hex" : "decimal";
    try {
      if (!input.value.trim()) (fromHex ? decimal : hex).value = "";
      else if (fromHex) decimal.value = (input.value.startsWith("-") ? -BigInt("0x" + input.value.slice(1)) : BigInt("0x" + input.value)).toString();
      else { const n = BigInt(input.value); hex.value = (n < 0n ? "-" : "") + (n < 0n ? -n : n).toString(16).toUpperCase().padStart(Number(hex.dataset.hexWidth || 0), "0"); }
    } catch (_) { /* Unfinished text stays for ordinary form parsing. */ }
  }
  const ids = value => value.split(/[,\s]+/).filter(Boolean);
  function zoneOrder(row) {
    const list = row.querySelector("[data-zone-order]"); if (!list) return;
    const input = row.querySelector(`[name="${list.dataset.zoneTarget}"]`), names=JSON.parse(input.dataset.choiceLabels||"{}"), values=ids(input.value), offset=Math.min(Number(list.dataset.offset||0),Math.max(0,Math.floor((values.length-1)/50)*50)); list.replaceChildren();
    values.slice(offset,offset+50).forEach((id, localIndex) => {
      const index=offset+localIndex;
      const item = document.createElement("div"); item.className = "ts-orderitem";
      const label = document.createElement("span"); label.textContent = names[id] ? names[id]+" · "+id : id; item.append(label);
      const actions = document.createElement("div"); actions.className = "ts-actions";
      const controls = row.closest("[data-profile-grid]").querySelector("template[data-zone-controls]").content.cloneNode(true);
      for (const button of controls.querySelectorAll("button")) button.dataset.zoneIndex = index;
      actions.append(controls);
      item.append(actions); list.append(item);
    });
    if(values.length>50){
      const pager=document.createElement("div");pager.className="ts-actions";
      for(const [step,label] of [[-50,"Previous"],[50,"Next"]]){const button=document.createElement("button");button.type="button";button.textContent=label;button.disabled=step<0?offset===0:offset+50>=values.length;button.addEventListener("click",()=>{list.dataset.offset=Math.max(0,offset+step);zoneOrder(row);});pager.append(button);}
      const count=document.createElement("span");count.textContent=(offset+1)+"–"+Math.min(offset+50,values.length)+" of "+values.length;pager.append(count);list.append(pager);
    }
  }
  for (const row of document.querySelectorAll("[data-profile-row]")) zoneOrder(row);
  document.addEventListener("click", event => {
    const target = event.target;
    if (target.closest("[data-admin-theme]")) { const value = document.body.dataset.theme === "dark" ? "light" : "dark"; theme(value); try { localStorage.setItem(themeKey,value); } catch (_) {} return; }
    const dirtyAction = target.closest("[data-unsaved-action]");
    if (dirtyAction) {
      const form = [...changedForms].find(f => f.isConnected), action = dirtyAction.dataset.unsavedAction;
      if (action === "discard") { changedForms.clear(); location.assign(pendingDestination); }
      else if (action === "save" && form) {
        const savedPath = new URL(form.action || location.href).pathname.replace(/\/edit$/, "");
        try { sessionStorage.setItem(navigationKey,JSON.stringify({savedPath,destination:pendingDestination})); } catch (_) {}
        form.requestSubmit();
      } else { document.querySelector("[data-unsaved-navigation]").remove(); form?.querySelector("input:not([type=hidden]),select,textarea")?.focus(); }
      return;
    }
    const add = target.closest("[data-profile-add]");
    if (add) {
      const grid = add.closest("[data-profile-grid]"), token = String(Number(grid.dataset.nextRow || 0)); grid.dataset.nextRow = String(Number(token) + 1);
      const template = document.createElement("template"); template.innerHTML = grid.querySelector("template[data-profile-template]").innerHTML.replaceAll("__row__",token);
      const row = template.content.firstElementChild; grid.insertBefore(row,grid.querySelector("template[data-profile-template]")); zoneOrder(row); row.querySelector("input:not([type=hidden])")?.focus(); formChanged({target:add}); return;
    }
    const remove = target.closest("[data-profile-remove]");
    if (remove) { formChanged({target:remove}); remove.closest("[data-profile-row]").remove(); return; }
    const order = target.closest("[data-zone-action]");
    if (order) {
      const row = order.closest("[data-profile-row]"), input = row.querySelector(`[name="${row.querySelector('[data-zone-order]').dataset.zoneTarget}"]`), values = ids(input.value), index = Number(order.dataset.zoneIndex), action = order.dataset.zoneAction;
      if (action === "remove") values.splice(index,1);
      else { const next = index + (action === "up" ? -1 : 1); if (next >= 0 && next < values.length) [values[index],values[next]] = [values[next],values[index]]; }
      input.value = values.join(", "); input.dispatchEvent(new Event("input",{bubbles:true})); return;
    }
    const link = target.closest("a[href]");
    if (link && event.button === 0 && !event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey && !link.hash && changedForms.size) { event.preventDefault(); navigate(link.href); }
  });
  document.addEventListener("change", event => {
    const input = event.target;
    if (input.matches("[data-number-choice]") && input.value !== "") { const decimal=input.closest(".ts-field").querySelector("[data-number-decimal]"); decimal.value=input.value;decimal.dispatchEvent(new Event("input",{bubbles:true})); }

  });
  document.addEventListener("input", event => {
    const input = event.target;
    numberPair(input);
    const named = input.closest(".ts-field")?.querySelector("[data-number-choice]");
    if (named) named.value = input.closest(".ts-field").querySelector("[data-number-decimal]").value;
        const row = input.closest("[data-profile-row]"); if (row) zoneOrder(row);
  });
  // Age labels update locally; observation times and full-date titles stay unchanged.
  function relativeTimes() {
    const now = Date.now();
    for (const node of document.querySelectorAll("time[datetime][title]")) {
      const delta = now - Date.parse(node.dateTime); if (!Number.isFinite(delta)) continue;
      const seconds = Math.floor(Math.abs(delta) / 1000), future = delta < 0;
      if (seconds < 60) { node.textContent = future ? "in a moment" : "just now"; continue; }
      const unit = seconds >= 86400 ? "day" : seconds >= 3600 ? "hour" : "minute";
      const count = Math.floor(seconds / ({day:86400,hour:3600,minute:60}[unit]));
      const text = count + " " + unit + (count === 1 ? "" : "s");
      node.textContent = future ? "in " + text : text + " ago";
    }
  }
  relativeTimes(); setInterval(relativeTimes,30000);
  // Same component on every administration page, clamped inside the viewport.
  function positionHelp(help) {
    const body = help.querySelector(".ts-helpbody"), rect = help.getBoundingClientRect();
    body.style.position = "fixed";
    body.style.left = Math.max(16,Math.min(rect.right - body.offsetWidth,innerWidth - body.offsetWidth - 16)) + "px";
    body.style.top = (rect.bottom + body.offsetHeight <= innerHeight - 16 ? rect.bottom : Math.max(16,rect.top - body.offsetHeight)) + "px";
  }
  for (const event of ["mouseover","focusin"]) document.addEventListener(event,event => {
    const help=event.target.closest(".ts-help"); if(help) positionHelp(help);
  });
  document.addEventListener("keydown",event => {if(event.key === "Escape") event.target.closest(".ts-help")?.blur();});
  document.addEventListener("click",async event => {
    const anchor = event.target.closest('.ts-editor-nav a[href^="#"]');
    if (anchor) {
      const section = document.querySelector(anchor.hash), advanced = section?.closest("details");
      if (advanced) advanced.open = true;
    }

  });

}
