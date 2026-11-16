"use strict";

const open25Timestamps = (() => {
  const exact = new Intl.DateTimeFormat("en-US", {
    year: "numeric", month: "short", day: "numeric",
    hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23",
    timeZone: "UTC", timeZoneName: "short"
  });

  function update(node, now) {
    const delta = now - Date.parse(node.dateTime);
    if (!Number.isFinite(delta)) return;
    const seconds = Math.floor(Math.abs(delta) / 1000);
    const future = delta < 0;
    if (seconds < 60) {
      node.textContent = seconds < 2 ? (future ? "in a moment" : "just now") : (future ? "in " + seconds + " s" : seconds + " s ago");
      return;
    }
    const unit = seconds >= 86400 ? "day" : seconds >= 3600 ? "hour" : "minute";
    const count = Math.floor(seconds / {day: 86400, hour: 3600, minute: 60}[unit]);
    const text = count + " " + unit + (count === 1 ? "" : "s");
    node.textContent = future ? "in " + text : text + " ago";
  }

  function updateAll() {
    const now = Date.now();
    for (const node of document.querySelectorAll("time[datetime][title]")) update(node, now);
  }

  updateAll();
  setInterval(updateAll, 30000);
  return {
    create(value) {
      const at = Date.parse(value);
      if (!Number.isFinite(at)) return document.createTextNode("—");
      const node = document.createElement("time");
      node.dateTime = value;
      node.title = exact.format(at);
      update(node, Date.now());
      return node;
    }
  };
})();
