"use strict";

// These conversations use the serving Application Server, independently of audio.
window.TrunkMessaging = {
  mount(container, {request, userID}) {
    const element = (tag, text, attributes = {}) => {
      const node = document.createElement(tag);
      if (text !== undefined) node.textContent = text;
      for (const [key, value] of Object.entries(attributes)) node.setAttribute(key, value);
      if (tag === "button" || tag === "fieldset") { node.style.maxWidth = "100%"; node.style.boxSizing = "border-box"; }
      if (tag === "fieldset") node.style.minWidth = "0";
      return node;
    };
    const button = text => element("button", text, {type: "button"});
    const heading = element("h2", "Messages"), identity = element("p"), unread = element("strong");
    const refreshButton = button("Refresh messages"), status = element("p", "", {role: "status"});
    const scope = element("p", "Your inbox on this Application Server. RF delivery pending.");
    const list = element("ul", undefined, {"aria-label": "Conversations"});
    const createForm = element("form", undefined, {id: "message-new"}), createFields = element("fieldset"), peopleFields = element("fieldset");
    createFields.append(element("legend", "New conversation"));
    const kind = element("select", undefined, {id: "message-kind"});
    kind.append(element("option", "Direct", {value: "direct"}), element("option", "Private group", {value: "group"}));
    const name = element("input", undefined, {id: "message-name", type: "text"});
    name.style.width = "100%"; name.style.maxWidth = "26em"; name.style.boxSizing = "border-box";
    const kindLabel = element("label", "Kind "); kindLabel.append(kind);
    const nameLabel = element("label", " Private group name "); nameLabel.append(name);
    peopleFields.append(element("legend", "People"));
    peopleFields.style.maxHeight = "12rem"; peopleFields.style.overflowY = "auto";
    createFields.append(kindLabel, nameLabel, element("p", "Choose one person for Direct, or people for a private group. New members see messages sent after they join."), peopleFields, element("button", "Create conversation"));
    createForm.append(createFields);
    const detail = element("section", undefined, {"aria-label": "Selected conversation"});
    const title = element("h3", "Select a conversation"), members = element("ul", undefined, {"aria-label": "Conversation members"});
    const manage = element("form", undefined, {id: "message-members"}), memberFields = element("fieldset");
    manage.append(memberFields, element("button", "Save members")); manage.hidden = true;
    const history = element("ol", undefined, {"aria-label": "Messages", tabindex: "0"});
    history.style.maxHeight = "24rem"; history.style.overflowY = "auto";
    const older = button("Load older messages"), read = button("Mark displayed messages read");
    const compose = element("form", undefined, {id: "message-compose"}), body = element("textarea", undefined, {id: "message-body", rows: "4"});
    body.style.width = "100%"; body.style.maxWidth = "48rem"; body.style.boxSizing = "border-box";
    const bodyLabel = element("label", "Message ", {for: "message-body"});
    compose.append(bodyLabel, body, element("p")); compose.lastChild.append(element("button", "Send message"));
    detail.append(title, members, manage, history, older, read, compose); detail.hidden = true;
    container.replaceChildren(heading, identity, unread, element("p"), scope, status, list, createForm, detail);
    container.style.minWidth = "0"; container.style.overflowWrap = "anywhere";
    container.children[3].append(refreshButton);

    let currentUser = "", epoch = 0, selection = 0, selectedID = "", currentThread = null, nextBefore = 0;
    let people = new Map(), threads = [], messages = new Map(), rows = new Map(), stream = null;
    let refreshWork = null, refreshAgain = false, refreshVersion = 0, creating = false;
    const drafts = new Map(), attempts = new Map(), sending = new Set(), memberDrafts = new Map(), savingMembers = new Set();
    const historyAfter = new Map();
    const say = value => { status.textContent = value; };
    const label = id => people.get(id)?.username || id;
    const threadName = thread => thread.name || (thread.kind === "direct" ? thread.members.filter(id => id !== currentUser).map(label).join(", ") || "Direct conversation" : "Private group");
    const path = id => "/api/messages/threads/" + encodeURIComponent(id);
    const same = (owner, token) => owner === epoch && token === selection;
    function renderPeople() {
      const focused = peopleFields.contains(document.activeElement) ? document.activeElement.value : "";
      const checked = new Set([...peopleFields.querySelectorAll("input:checked")].map(node => node.value));
      peopleFields.replaceChildren(element("legend", "People"));
      for (const person of people.values()) {
        const checkbox = element("input", undefined, {type: "checkbox", value: person.id});
        checkbox.checked = checked.has(person.id);
        const personLabel = element("label"); personLabel.append(checkbox, document.createTextNode(" " + person.username + " (" + person.id + ")"));
        const line = element("p"); line.append(personLabel); peopleFields.append(line);
        if (person.id === focused) checkbox.focus();
      }
      identity.textContent = "Signed in as " + label(currentUser) + " (" + currentUser + ").";
    }
    function renderMembers() {
      members.replaceChildren(...currentThread.members.map(id => element("li", label(id) + " (" + id + ")")));
      manage.hidden = currentThread.kind !== "group" || currentThread.created_by !== currentUser;
      if (manage.hidden) return;
      const focused = memberFields.contains(document.activeElement) ? document.activeElement.value : "";
      const chosen = memberDrafts.get(selectedID) || new Set(currentThread.members);
      memberFields.replaceChildren(element("legend", "Manage members"), element("p", "The creator stays a member. New members see messages sent after they join."));
      memberFields.append(element("p", "Creator: " + label(currentThread.created_by) + " (" + currentThread.created_by + ")"));
      for (const id of new Set([...people.keys(), ...currentThread.members])) {
        if (id === currentThread.created_by) continue;
        const checkbox = element("input", undefined, {type: "checkbox", value: id}); checkbox.checked = chosen.has(id);
        checkbox.addEventListener("change", () => memberDrafts.set(selectedID, new Set([...memberFields.querySelectorAll("input:checked")].map(node => node.value))));
        const line = element("p"), memberLabel = element("label");
        memberLabel.append(checkbox, document.createTextNode(" " + label(id) + " (" + id + ")")); line.append(memberLabel); memberFields.append(line);
        if (id === focused) checkbox.focus();
      }
    }
    function clearConversation() {
      if (selectedID) { drafts.set(selectedID, body.value); attempts.delete(selectedID); memberDrafts.delete(selectedID); }
      selectedID = ""; selection++; currentThread = null; nextBefore = 0; messages = new Map(); rows = new Map();
      body.value = ""; history.replaceChildren(); members.replaceChildren(); memberFields.replaceChildren(); manage.hidden = true;
      title.textContent = "Select a conversation"; detail.hidden = true;
      say("Conversation no longer available.");
    }
    function renderThreads() {
      const focusedID = list.contains(document.activeElement) ? document.activeElement.dataset.thread : "";
      list.replaceChildren();
      unread.textContent = threads.reduce((total, thread) => total + thread.unread_count, 0) + " unread messages";
      if (!threads.length) list.append(element("li", "No conversations yet."));
      for (const thread of threads) {
        const open = button(threadName(thread) + " · " + thread.unread_count + " unread");
        open.dataset.thread = thread.id; open.setAttribute("aria-pressed", String(thread.id === selectedID));
        open.addEventListener("click", () => openThread(thread.id));
        const item = element("li"); item.append(open); list.append(item);
        if (thread.id === focusedID) open.focus();
      }
    }
    function acceptThreads(values) {
      for (const thread of values) historyAfter.set(thread.id, Math.max(historyAfter.get(thread.id) || 0, thread.history_after_id));
      threads = values;
      const listed = threads.find(thread => thread.id === selectedID);
      if (selectedID && !listed) clearConversation();
      else if (listed && (!currentThread || historyAfter.get(listed.id) > currentThread.history_after_id)) {
        // Apply an observed membership boundary before another history request:
        // that request may fail while old cached bodies are no longer visible.
        currentThread = {...listed, history_after_id: historyAfter.get(listed.id)}; messages = new Map(); rows = new Map(); nextBefore = 0;
        history.replaceChildren(); older.hidden = true; renderMembers();
      }
      renderThreads();
    }
    function renderMessages(reset, prepend) {
      const top = history.scrollTop, height = history.scrollHeight;
      const atBottom = height - top - history.clientHeight < 24;
      if (reset) { history.replaceChildren(); rows = new Map(); }
      for (const message of [...messages.values()].sort((a, b) => a.id - b.id)) {
        let row = rows.get(message.id);
        if (!row) {
          row = element("li"); row.dataset.message = String(message.id);
          const sender = element("p", (message.sender_name || message.sender_id) + " · " + message.created_at);
          const content = element("p", message.body); content.style.whiteSpace = "pre-wrap"; content.style.overflowWrap = "anywhere";
          row.append(sender, content); rows.set(message.id, row);
        }
        history.append(row);
      }
      older.hidden = !nextBefore;
      if (reset || atBottom && !prepend) history.scrollTop = history.scrollHeight;
      else if (prepend) history.scrollTop = top + history.scrollHeight - height;
      else history.scrollTop = top;
    }
    async function markRead(owner = epoch, token = selection) {
      if (!selectedID || !messages.size) return "No messages displayed.";
      if (document.hidden) return "Open this page to mark displayed messages read.";
      const id = selectedID, displayed = Math.max(...messages.keys());
      if (displayed <= (currentThread?.read_through_id || 0)) return "Displayed messages are already marked read.";
      let result;
      try { result = await request(path(id) + "/read", "POST", {message_id: displayed}); }
      catch (error) { if (same(owner, token) && [403, 404].includes(error.status)) { clearConversation(); renderThreads(); } throw error; }
      if (!same(owner, token) || selectedID !== id) return;
      currentThread.read_through_id = result.read_through_id;
      await loadThreads(owner);
      return "Displayed messages marked read.";
    }
    async function loadThread({reset = false, before = 0} = {}) {
      const owner = epoch, token = selection, id = selectedID;
      if (!id) return;
      let result;
      try { result = await request(path(id) + "?before_id=" + before + "&limit=50"); }
      catch (error) { if (same(owner, token) && [403, 404].includes(error.status)) { clearConversation(); renderThreads(); } throw error; }
      if (!same(owner, token) || selectedID !== id) return;
      // A rejoined membership starts a new visible history. An older in-flight
      // response cannot restore the preceding membership's cached messages.
      if (result.thread.history_after_id < (historyAfter.get(id) || 0)) return;
      historyAfter.set(id, result.thread.history_after_id);
      const replace = reset || currentThread && result.thread.history_after_id !== currentThread.history_after_id;
      currentThread = result.thread;
      if (replace) messages = new Map();
      for (const message of result.messages) messages.set(message.id, message);
      // Refreshing the latest page must retain the cursor for already-loaded history.
      if (replace || before) nextBefore = result.next_before_id;
      title.textContent = threadName(currentThread);
      renderMembers();
      detail.hidden = false;
      renderMessages(replace, Boolean(before));
      await markRead(owner, token);
    }
    async function openThread(id) {
      if (selectedID) drafts.set(selectedID, body.value);
      selectedID = id; selection++; currentThread = null; nextBefore = 0; messages = new Map();
      body.value = drafts.get(id) || ""; title.textContent = "Loading conversation…";
      history.replaceChildren(); members.replaceChildren(); memberFields.replaceChildren(); manage.hidden = true; detail.hidden = false; renderThreads();
      const owner = epoch, token = selection;
      try { await loadThread({reset: true}); if (same(owner, token)) say("Conversation loaded. RF delivery pending."); }
      catch (error) { if (same(owner, token)) say(error.message); }
    }
    async function loadThreads(owner = epoch) {
      let result;
      try { result = await request("/api/messages/threads"); }
      catch (error) { if (owner === epoch && [401, 403, 404].includes(error.status)) { clearConversation(); threads = []; renderThreads(); } throw error; }
      if (owner !== epoch) return;
      acceptThreads(result.threads);
    }
    async function refresh() {
      if (!currentUser) return;
      if (refreshWork) { refreshAgain = true; return refreshWork; }
      const owner = epoch, version = ++refreshVersion;
      refreshWork = (async () => {
        do {
          refreshAgain = false;
          const [directory, inbox] = await Promise.allSettled([request("/api/messages/people"), request("/api/messages/threads")]);
          if (owner !== epoch) return;
          if (directory.status === "fulfilled") { people = new Map(directory.value.people.map(person => [person.id, person])); renderPeople(); }
          if (inbox.status === "fulfilled") acceptThreads(inbox.value.threads);
          if (directory.status === "rejected") throw directory.reason;
          if (inbox.status === "rejected") throw inbox.reason;
          if (selectedID && !document.hidden) await loadThread();
        } while (refreshAgain && owner === epoch);
      })();
      try { await refreshWork; }
      catch (error) { if (owner === epoch && [401, 403, 404].includes(error.status)) { clearConversation(); threads = []; renderThreads(); } throw error; }
      finally { if (version === refreshVersion) refreshWork = null; }
    }
    function refreshFromEvent() { refresh().catch(error => say(error.message)); }
    async function setUser(id) {
      id = id || "";
      if (id === currentUser && epoch) return;
      currentUser = id; epoch++; selection++; refreshVersion++; refreshWork = null; refreshAgain = false; creating = false;
      const owner = epoch;
      stream?.close(); stream = null; selectedID = ""; currentThread = null; nextBefore = 0;
      people = new Map(); threads = []; messages = new Map(); rows = new Map(); historyAfter.clear(); drafts.clear(); attempts.clear(); sending.clear(); memberDrafts.clear(); savingMembers.clear();
      body.value = ""; detail.hidden = true; history.replaceChildren(); members.replaceChildren(); title.textContent = "Select a conversation";
      createForm.reset(); manage.hidden = true; memberFields.replaceChildren(); list.replaceChildren(); peopleFields.replaceChildren(element("legend", "People"));
      identity.textContent = id ? "Loading your inbox…" : "Sign in with a person account to use messages.";
      unread.textContent = ""; createForm.hidden = !id; say("");
      if (!id) return;
      try { await refresh(); }
      catch (error) { say(error.message); }
      if (owner !== epoch) return;
      if (typeof EventSource === "function") {
        const source = stream = new EventSource("/api/messages/events");
        let opened = false;
        source.addEventListener("open", () => { if (opened && owner === epoch) { say("Live message updates connected."); refreshFromEvent(); } opened = true; });
        source.addEventListener("inbox", () => { if (owner === epoch) refreshFromEvent(); });
        source.addEventListener("error", () => { if (owner === epoch) say("Live message updates disconnected. Use Refresh messages."); });
      }
    }
    body.addEventListener("input", () => { if (selectedID) drafts.set(selectedID, body.value); });
    compose.addEventListener("submit", async event => {
      event.preventDefault();
      const id = selectedID, owner = epoch;
      if (!id) { say("Select a conversation first."); return; }
      if (sending.has(id)) { say("This message is still being sent."); return; }
      const text = body.value;
      let attempt = attempts.get(id);
      if (!attempt || attempt.body !== text) {
        attempt = {client_id: crypto.randomUUID(), body: text}; attempts.set(id, attempt);
      }
      sending.add(id); say("Sending message…");
      try {
        const result = await request(path(id) + "/messages", "POST", attempt);
        if (owner !== epoch) return;
        attempts.delete(id);
        if (drafts.get(id) === text || selectedID === id && body.value === text) {
          drafts.delete(id); if (selectedID === id) body.value = "";
        }
        say("Message " + result.message.id + " stored. RF delivery pending.");
        try { await refresh(); }
        catch (error) { if (owner === epoch) say("Message " + result.message.id + " stored. Refresh failed: " + error.message); }
      } catch (error) { if (owner === epoch) {
        if (selectedID === id && [403, 404].includes(error.status)) { clearConversation(); renderThreads(); say(error.message); }
        else say(error.message + " Your draft is kept. Select Send message to retry.");
      } }
      finally { if (owner === epoch) sending.delete(id); }
    });
    createForm.addEventListener("submit", async event => {
      event.preventDefault();
      const owner = epoch;
      if (creating) { say("The conversation is still being created."); return; }
      const members = [...peopleFields.querySelectorAll("input:checked")].map(node => node.value);
      creating = true; say("Creating conversation…");
      try {
        const result = await request("/api/messages/threads", "POST", {kind: kind.value, name: name.value, members});
        if (owner !== epoch) return;
        createForm.reset();
        await loadThreads(owner); await openThread(result.thread.id);
      } catch (error) { if (owner === epoch) say(error.message); }
      finally { if (owner === epoch) creating = false; }
    });
    manage.addEventListener("submit", async event => {
      event.preventDefault();
      const owner = epoch, id = selectedID;
      if (!id || savingMembers.has(id)) { say("Member changes are still being saved."); return; }
      const chosen = [...memberFields.querySelectorAll("input:checked")].map(node => node.value);
      chosen.push(currentThread.created_by); memberDrafts.set(id, new Set(chosen)); savingMembers.add(id);
      try {
        await request(path(id) + "/members", "PUT", {members: chosen});
        if (owner !== epoch) return;
        memberDrafts.delete(id); await loadThreads(owner);
        if (selectedID === id) await loadThread({reset: true});
        say("Members saved. New members see messages sent after they join.");
      } catch (error) { if (owner === epoch) say(error.message + " Your member choices are kept."); }
      finally { if (owner === epoch) savingMembers.delete(id); }
    });
    refreshButton.addEventListener("click", () => refresh().then(() => say("Messages refreshed. RF delivery pending.")).catch(error => say(error.message)));
    older.addEventListener("click", () => { if (nextBefore) loadThread({before: nextBefore}).catch(error => say(error.message)); });
    read.addEventListener("click", () => markRead().then(value => { if (value) say(value); }).catch(error => say(error.message)));
    const visible = () => { if (!document.hidden) refreshFromEvent(); };
    document.addEventListener("visibilitychange", visible);
    const close = () => { stream?.close(); document.removeEventListener("visibilitychange", visible); };
    window.addEventListener("pagehide", close, {once: true});
    setUser(userID);
    return {refresh, setUser, close};
  }
};
