"use strict";

// Plain testing pages use the existing authenticated browser-audio contract.
(async function () {
  const page = document.getElementById("audio-test");
  if (!page) return;
  const ptt = page.dataset.kind === "ptt";
  const escape = value => String(value ?? "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"})[c]);
  const node = id => document.getElementById(id);
  const measurementUnits = {
    tx_frequency_hz: ["Transmit frequency (MHz)", 1e6], rx_frequency_hz: ["Receive frequency (MHz)", 1e6],
    base_frequency_hz: ["Base frequency (MHz)", 1e6], uplink_offset_hz: ["Uplink offset (MHz)", 1e6],
    channel_spacing_hz: ["Channel spacing (kHz)", 1000], bandwidth_hz: ["Bandwidth (kHz)", 1000],
    rx_offset_hz: ["Receive calibration (Hz)", 1], tx_offset_hz: ["Transmit calibration (Hz)", 1],
    lifetime_ms: ["Call lifetime (seconds)", 1000], call_lifetime_ms: ["Call lifetime (seconds)", 1000],
    allocated_ms: ["Allocated time (seconds)", 1000], speech_ms: ["Speech time (seconds)", 1000],
    latency_ms: ["Latency (ms)", 1], jitter_ms: ["Jitter (ms)", 1], rtt_ms: ["Round-trip time (ms)", 1]
  };
  // W3C getStats jitter/delays use seconds. Buffer delay is a cumulative
  // sample/frame total, not an instantaneous or end-to-end latency reading.
  const rtcUnits = {
    jitter: ["Jitter (ms)", .001],
    jitterBufferDelay: ["Cumulative jitter-buffer delay (ms)", .001],
    jitterBufferTargetDelay: ["Cumulative target jitter-buffer delay (ms)", .001],
    jitterBufferMinimumDelay: ["Cumulative minimum jitter-buffer delay (ms)", .001],
    roundTripTime: ["Round-trip time (ms)", .001], totalRoundTripTime: ["Cumulative round-trip time (ms)", .001]
  };
  function readable(value, rtc = false) {
    const format = item => {
      if (Array.isArray(item)) return item.map(format);
      if (!item || typeof item !== "object") return item;
      return Object.fromEntries(Object.entries(item).map(([key, value]) => {
        const unit = measurementUnits[key] || (rtc ? rtcUnits[key] : undefined);
        return [unit ? unit[0] : key, unit && typeof value === "number" ? value / unit[1] : format(value)];
      }));
    };
    return JSON.stringify(format(value), null, 2);
  }
  const features = [
    ["Group voice", "Speak to a talkgroup and hear its calls.", "Implemented"],
    ["Encrypted voice", "Use the group's assigned key to protect voice.", "Partial"],
    ["Transmit strapping", "Program a group for clear voice, encrypted voice, or an operator choice.", "Implemented"],
    ["Talkgroup admission", "Accept clear calls, encrypted calls, or both according to the group's settings.", "Implemented"],
    ["Receive encryption", "Use each received call's key while keeping the transmit selection separate.", "Implemented with matching loaded keys"],
    ["Assigned transmit key", "Choose the algorithm and key number used for outgoing encrypted calls.", "Implemented in System Administration"],
    ["Operator key selection", "Choose a loaded transmit key or the talkgroup's assigned key.", "Implemented"],
    ["Clear call reception", "Hear permitted clear calls and see Clear in the call display.", "Implemented"],
    ["Transmit permissions", "Apply the person's assigned transmit permissions.", "Implemented"],
    ["Emergency", "Declare an emergency and track acknowledgement and clearing.", "Partial"],
    ["Radio check", "Ask a selected radio to respond.", "Implemented"],
    ["Call alert", "Send an alert and read the radio's response.", "Implemented"],
    ["Browser location", "Send this device's current position.", "Implemented"],
    ["Recordings", "Record the received and transmitted audio during this session.", "Implemented"],
    ["DTMF", "Send keypad tones during an accepted transmission.", "Implemented"],
    ["WAV announcements", "Transmit an audio file to the chosen groups.", "Implemented"],
    ["Spoken announcements", "Have the server speak entered words.", "Implemented"],
    ["Call history", "Read past calls and their delivery outcomes.", "Implemented"],
    ["Affiliated radios", "Read radios currently associated with the selected talkgroup.", "Implemented"],
    ["Private calls", "Speak directly to one radio.", "Not implemented"],
    ["Web conversations", "Exchange written messages with people and private groups in both apps.", "Implemented"],
    ["RF text messages", "Deliver written messages to physical radios and talkgroups.", "Not implemented; RF delivery pending"],
    ["Coded messages", "Send a numbered message to a physical radio and read its acknowledgment.", "Implemented; radio verification pending"],
    ["Coded status", "Send and receive numbered status reports.", "Partial; administration has received reports"],
    ["RF location", "Receive a physical radio's reported position.", "Not implemented"],
    ["Location map", "Show reported positions on a map.", "Not implemented"],
    ["On-air caller names", "Send the System radio name to listening radios during Phase 1 trunked calls.", "Implemented; radio display test pending"],
    ["Received on-air names", "Receive a radio's transmitted name.", "Not implemented"],
    ["Remote monitor", "Ask an authorized radio to transmit its microphone for a bounded time.", "Not implemented"],
    ["Subscriber emergency reset", "Ask a physical radio to leave emergency mode.", "Not implemented"],
    ["Alert tones", "Choose and transmit a programmed alert sound.", "Not implemented"],
    ["Checklist paging", "Send a prepared sequence of pages.", "Not implemented"],
    ["Radio regrouping", "Move supported radios into a temporary group.", "Not implemented"],
    ["Radio key updates", "Distribute replacement keys to physical radios.", "Deferred"],
    ["Radio authentication", "Verify a physical radio through the selected authentication procedure.", "Deferred"],
    ["Radio programming", "Send programming changes to physical radios.", "Not implemented"],
    ...(ptt ? [
      ["Programmed zones", "Choose a group from the person's saved zones.", "Implemented"],
      ["Saved preferences", "Remember the selected talkgroup and listening settings.", "Implemented"],
      ["Foreground scan", "Hear the first eligible call among checked groups.", "Implemented"],
      ["Background phone operation", "Maintain audio while a phone is locked or another app is open.", "Outside current scope"],
    ] : [
      ["Multiple listening groups", "Hear several subscribed groups together.", "Implemented"],
      ["Multiple transmit groups", "Transmit to the entered list of groups.", "Implemented"],
      ["Software patches", "Join permitted talkgroups until the patch is ended.", "Implemented"],
      ["Patch membership editing", "Change a patch's groups while idle.", "Implemented; idle-only"],
      ["Console override", "Interrupt another transmission under an assigned policy.", "Not implemented"],
    ]),
    ["Audio device selection", "Choose this computer’s microphone and speaker.", "Implemented; browser permission and support apply"],
    ["Automatic audio connection", "Connect when the app opens and reconnect after a temporary interruption.", "Implemented; Enable audio appears when the browser requires interaction"],
    ["Transmit limits", "End transmissions according to the configured time policy.", "Partial; existing server limit applies"],
    ["Secure receive permission", "Assign permission to hear decrypted speech independently of transmit permission.", "Not implemented separately"],
    ["Site delivery", "Read which Sites are carrying the actual call.", "Partial; use Read controller results"],
  ];
  page.innerHTML = `
    <p id="test-identity">Loading account…</p>
    <p><button id="test-refresh" type="button">Reload account and groups</button>
      <button id="test-connect" type="button" hidden>Enable audio</button>
      <strong id="test-connection" role="status">Connecting</strong></p>
    <p id="test-result" role="status"></p>
    <fieldset><legend>Audio devices</legend>
      <p><button id="test-devices" type="button">Allow microphone and list devices</button>
        <label>Microphone <select id="test-microphone"><option value="">Browser default</option></select></label>
        <label>Speaker <select id="test-speaker"><option value="">Browser default</option></select></label>
        <button id="test-speaker-choose" type="button">Choose another speaker</button>
        <button id="test-speaker-resume" type="button">Resume speaker</button></p>
      <p id="test-devices-result" role="status">Device choices stay in this browser. Audio connects automatically.</p>
    </fieldset>
    <fieldset><legend>Transmit</legend>
      ${ptt ? '<label>Programmed zone <select id="test-zone"></select></label> ' : ''}
      <label>Talkgroup <input id="test-group" type="number" list="test-groups"></label><datalist id="test-groups"></datalist>
      ${ptt ? '' : '<label>Additional talkgroups, separated by commas <input id="test-extra-groups"></label>'}
      <label><input id="test-secure" type="checkbox">Request encrypted transmission</label>
      <label>Transmit key <select id="test-key"><option value="0:0">Talkgroup's assigned key</option></select></label>
      <p><button id="test-ptt" type="button">Hold to talk</button> <button id="test-release" type="button">Release PTT</button> <strong id="test-transmit">Idle</strong></p>
      <p>Microphone (% full scale) <meter id="test-mic-level" min="0" max="100" value="0"></meter> Received audio (% full scale) <meter id="test-rx-level" min="0" max="100" value="0"></meter></p>
      <p>Space bar transmits outside message and form controls. Changing group or leaving the page releases PTT.</p>
      <p>Keypad: ${'123456789*0#'.split('').map(key => `<button type="button" data-tone="${key}">${key}</button>`).join(' ')}</p>
    </fieldset>
    <fieldset><legend>Receive</legend>
      ${ptt ? '<p><label><input id="test-scan" type="checkbox">Scan checked groups while this page is open</label></p>' : ''}
      <table border="1" cellpadding="4"><thead><tr><th>Listen</th><th>Talkgroup</th><th>Volume (%)</th><th>Speaker</th><th>Call</th></tr></thead><tbody id="test-receive"></tbody></table>
      ${ptt ? '<p><button id="test-save-preferences" type="button">Save preferences</button> Saves the talkgroup, listening choices, volume and left/right settings to your account.</p>' : ''}
    </fieldset>
    <fieldset><legend>Announcements</legend>
      <p><label>8 kHz mono PCM WAV file <input id="test-wav" type="file" accept="audio/wav,.wav"></label> <button id="test-send-wav" type="button">Transmit WAV</button></p>
      <p><label>Words to speak <textarea id="test-speech" rows="3" cols="60"></textarea></label> <button id="test-send-speech" type="button">Transmit speech</button> <button id="test-cancel-file" type="button">Cancel announcement</button></p>
    </fieldset>
    <fieldset><legend>Test recording</legend>
      <p><button id="test-record" type="button">Start recording</button> <button id="test-record-stop" type="button">Stop recording</button> <button id="test-record-delete" type="button">Delete recording</button></p>
      <p>Records received speaker audio and microphone audio admitted for transmission. The recording stays in this browser until downloaded or the page closes.</p><div id="test-recording"></div>
    </fieldset>
    <fieldset><legend>Emergency and radio actions</legend>
      <p><button id="test-emergency" type="button">Declare own emergency</button> <label>Incident ID <input id="test-incident"></label> <button id="test-ack" type="button">Acknowledge incident</button> <button id="test-clear" type="button">Clear incident</button></p>
      <form id="test-command"><label>Radio ID <input name="radio_id" type="number"></label> <label>Command <select name="action"><option value="radio_check">Radio check</option><option value="page">Call alert</option><option value="message">Coded message</option></select></label> <label>Message code <input name="code" type="number" value="0"></label> <button>Send command</button></form>
      <p><button id="test-location" type="button">Send my location once</button></p>
      ${ptt ? '' : `<form id="test-patch"><label>Patch name <input name="name"></label> <label>Member groups, separated by commas <input name="talkgroups"></label> <label>Algorithm number <input name="algorithm_id" type="number" value="0"></label> <label>Key number <input name="key_id" type="number" value="0"></label> <button>Start patch</button></form>
      <p><label>Load existing patch <select id="test-patch-select"><option value="">Read controller results to load patches</option></select></label></p>
      <p>Read controller results, then choose a patch to load its current fields.</p><p><label>Existing patch ID <input id="test-patch-id"></label> <label>Previous members <input id="test-patch-previous"></label> <label>New members <input id="test-patch-members"></label> <button id="test-patch-edit" type="button">Change idle patch members</button> <button id="test-patch-end" type="button">End patch</button></p>`}
      <p><button id="test-read-results" type="button">Read controller results</button> <button id="test-command-results" type="button">Read radio command results</button> <button id="test-units" type="button">Read affiliated radios</button> <button id="test-history" type="button">Read call history</button> <button id="test-stats" type="button">Read audio statistics</button></p><pre id="test-controller-results"></pre>
    </fieldset>
    <fieldset><legend>Feature list</legend><p>Implementation describes existing software. Radio and audio qualification of these new testing pages is pending.</p>
      <table border="1" cellpadding="4"><thead><tr><th>Feature</th><th>What it does</th><th>Implementation</th><th>Verification</th></tr></thead><tbody>${features.map(row => '<tr>' + [...row, row[0] === 'Web conversations' ? 'Browser and authenticated API checks' : row[0] === 'Saved preferences' ? 'Browser save/reload and profile API checks' : row[0] === 'Audio device selection' ? 'Browser checks; physical-device test pending' : row[2].startsWith('Not implemented') || row[2] === 'Deferred' || row[2] === 'Outside current scope' ? 'Pending implementation' : 'New-page radio test pending'].map(value => '<td>' + escape(value) + '</td>').join('') + '</tr>').join('')}</tbody></table>
    </fieldset>
    <p><button id="test-clear-log" type="button">Clear event log</button></p><pre id="test-log"></pre>`;

  let state = {}, csrf = "", connection = null, recording = null, recordingURL = "", announcement = null, messaging = null;
  let audioWanted = true, accountReady = false, audioWait = "", retryTimer, retryDelay = 1000;
  let zones = [], groups = [], patches = [];
  const log = value => { node("test-log").textContent = (new Date().toISOString() + " " + value + "\n" + node("test-log").textContent).split("\n").slice(0, 200).join("\n"); };
  const result = value => { node("test-result").textContent = value; log(value); };
  async function request(path, method = "GET", body, signal) {
    const headers = {};
    if (method !== "GET") headers["X-CSRF-Token"] = csrf;
    if (body instanceof Blob) headers["Content-Type"] = "audio/wav";
    if (body !== undefined && !(body instanceof Blob)) { headers["Content-Type"] = "application/json"; body = JSON.stringify(body); }
    const response = await fetch(path, {method, headers, body, signal, credentials: "same-origin", cache: "no-store"});
    const value = await response.json();
    if (!response.ok) { const error = new Error(value.error || "Request failed: " + response.status); error.status = response.status; throw error; }
    return value;
  }
  const selected = () => Number(node("test-group").value);
  const numbers = text => text.split(",").filter(value => value.trim() !== "").map(Number);
  const targets = () => [selected(), ...(ptt ? [] : numbers(node("test-extra-groups").value))];
  const transmitKey = () => {
    const [algorithm_id, key_id] = node("test-key").value.split(":").map(Number);
    return {algorithm_id, key_id};
  };
  function loadKeys(keys) {
    const select = node("test-key"), selected = select.value;
    const choices = new Map([["0:0", "Talkgroup's assigned key"], [selected, select.selectedOptions[0].textContent]]);
    for (const key of keys || []) choices.set(`${key.algorithm_id}:${key.key_id}`, `Algorithm ${key.algorithm_id}, Key ${key.key_id}`);
    select.replaceChildren(...[...choices].map(([value, label]) => new Option(label, value)));
    select.value = selected;
  }
  const deviceResult = value => { node("test-devices-result").textContent = value; log(value); };
  const deviceFields = ["microphone", "speaker"];
  try {
    const saved = JSON.parse(localStorage.getItem("trunksystem-audio-devices") || "{}");
    for (const kind of deviceFields) if (typeof saved[kind]?.id === "string" && saved[kind].id) {
      node("test-" + kind).add(new Option(saved[kind].label || "Saved device", saved[kind].id));
      node("test-" + kind).value = saved[kind].id;
    }
  } catch (_) { /* Device preferences are optional browser storage. */ }
  function saveDevices() {
    const saved = Object.fromEntries(deviceFields.map(kind => {
      const field = node("test-" + kind);
      return [kind, {id: field.value, label: field.selectedOptions[0].textContent}];
    }));
    try { localStorage.setItem("trunksystem-audio-devices", JSON.stringify(saved)); }
    catch (_) { deviceResult("Device choices work for this page; this browser could not save them."); }
  }
  function stopMicrophone(c) {
    c.micSource?.disconnect(); c.micSource = null;
    c.mic?.getTracks().forEach(track => { track.onended = null; track.stop(); }); c.mic = null;
  }
  async function useMicrophone(c) {
    release(c); stopMicrophone(c);
    const request = c.micRequest = (c.micRequest || 0) + 1, id = node("test-microphone").value;
    c.microphoneID = id;
    let mic;
    try { mic = await navigator.mediaDevices.getUserMedia({audio: {channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true, ...(id ? {deviceId: {exact: id}} : {})}, video: false}); }
    catch (error) { if (c !== connection || request !== c.micRequest) return; throw error; }
    if (c !== connection || request !== c.micRequest) { mic.getTracks().forEach(track => track.stop()); return; }
    c.mic = mic;
    mic.getAudioTracks().forEach(track => { track.onended = () => { if (c === connection && c.mic === mic) { release(c); stopMicrophone(c); deviceResult("Microphone disconnected. Choose a microphone before transmitting."); } }; });
    c.micSource = c.audio.createMediaStreamSource(mic);
    c.micSource.connect(c.mix); c.micSource.connect(c.recordTX); c.micSource.connect(c.micMeter);
    deviceResult("Microphone ready: " + (mic.getAudioTracks()[0].label || "Browser default"));
  }
  const outputPlayers = () => [connection?.player, node("test-recording").querySelector("audio")].filter(Boolean);
  function muteOutput() { outputPlayers().forEach(player => { player.muted = true; player.pause(); }); }
  let speakerWork = Promise.resolve();
  function useSpeaker() {
    const id = node("test-speaker").value, c = connection;
    const playing = new Set(outputPlayers().filter(player => !player.paused)); muteOutput();
    // Serialize native output changes so an older selection cannot finish last.
    return speakerWork = speakerWork.catch(() => {}).then(async () => {
      if (id !== node("test-speaker").value || c !== connection) return;
      if (!outputPlayers().length) { deviceResult("Speaker selected. Audio will use it when connected."); return; }
      try {
        for (const player of outputPlayers()) {
          if (id && !player.setSinkId) throw new DOMException("This browser cannot select a speaker. Choose Browser default.", "NotSupportedError");
          if (player.setSinkId) await player.setSinkId(id);
          if (id !== node("test-speaker").value || c !== connection) return;
          if (player !== c?.player && !player.getAttribute("src") && recordingURL) player.src = recordingURL;
          player.muted = false;
          if (player === c?.player || playing.has(player)) await player.play();
        }
        deviceResult("Speaker ready: " + node("test-speaker").selectedOptions[0].textContent);
      } catch (error) {
        if (id !== node("test-speaker").value || c !== connection) return;
        muteOutput();
        const recorded = node("test-recording").querySelector("audio"); recorded?.removeAttribute("src"); recorded?.load();
        throw Object.assign(new Error("Speaker unavailable: " + error.message), {name: error.name});
      }
    });
  }
  async function listDevices() {
    const devices = await navigator.mediaDevices.enumerateDevices();
    for (const [kind, type] of [["microphone", "audioinput"], ["speaker", "audiooutput"]]) {
      const field = node("test-" + kind), selected = field.value, label = field.selectedOptions[0].textContent;
      const choices = new Map([["", "Browser default"]]);
      for (const device of devices) if (device.kind === type && device.deviceId) choices.set(device.deviceId, device.label || "Unnamed " + kind);
      if (selected && !choices.has(selected)) choices.set(selected, label);
      field.replaceChildren(...[...choices].map(([value, text]) => new Option(text, value))); field.value = selected;
    }
    deviceResult("Device list loaded. Choose a microphone and speaker.");
    const c = connection;
    if (c?.microphoneID && !devices.some(device => device.kind === "audioinput" && device.deviceId === c.microphoneID)) {
      release(c); stopMicrophone(c); deviceResult("Selected microphone is no longer available. Choose a microphone before transmitting.");
    }
    if (outputPlayers().length && node("test-speaker").value && !devices.some(device => device.kind === "audiooutput" && device.deviceId === node("test-speaker").value)) {
      muteOutput(); const recorded = node("test-recording").querySelector("audio"); recorded?.removeAttribute("src"); recorded?.load();
      deviceResult("Selected speaker is no longer available. Choose a speaker, then Resume speaker.");
    }
  }
  const send = (c, value) => { if (c?.socket?.readyState === WebSocket.OPEN) c.socket.send(JSON.stringify(value)); };
  const listened = id => !!node("listen-" + id)?.checked && (!ptt || node("test-scan").checked || id === selected());
  function subscriptions() {
    if (!connection) return;
    send(connection, {type: "subscribe", talkgroups: (connection.ticket?.receive_groups || []).filter(listened)});
    applyReceive(connection);
  }
  function applyReceive(c) {
    const winner = ptt ? [...c.calls.values()].find(call => listened(call.talkgroup_id) && !call.audio_unavailable)?.talkgroup_id : 0;
    for (const [id, receive] of c.receive) {
      const call = c.calls.get(id);
      if (ptt && (!listened(id) || call?.audio_unavailable)) receive.admitted = false;
      else if (ptt && winner) receive.admitted = id === winner;
      // Control End may arrive before queued RTP. Preserve admitted playout
      // through idle until another call or a changed subscription replaces it.
      if (!node("volume-" + id)) { receive.gain.gain.value = 0; receive.admitted = false; continue; }
      receive.gain.gain.value = listened(id) && !call?.audio_unavailable && (!ptt || receive.admitted) ? Number(node("volume-" + id).value) / 100 : 0;
      receive.pan.pan.value = Number(node("pan-" + id).value);
      const label = node("call-" + id);
      if (label) label.textContent = call ? `Radio ${call.source_id} ${call.source_alias || ""} · ${call.encrypted ? `Encrypted · Algorithm ${call.algorithm_id}, Key ${call.key_id}` : "Clear"}${call.audio_unavailable ? " · " + call.audio_unavailable : ""}` : "Idle";
    }
  }
  function stopTone(c = connection) {
    if (!c?.tone) return;
    c.tone.forEach(oscillator => { oscillator.stop(); oscillator.disconnect(); }); c.tone = null;
  }
  function release(c = connection) {
    if (!c) return;
    c.held = false;
    if (c.track) c.track.enabled = false;
    if (c.recordTX) c.recordTX.gain.value = 0;
    stopTone(c); clearInterval(c.renew);
    if (c.requestID) send(c, {type: "ptt_stop", request_id: c.requestID});
    c.requestID = "";
    if (c === connection) node("test-transmit").textContent = "Idle";
  }
  function startPTT() {
    const c = connection;
    if (!c || c.peer?.connectionState !== "connected") { result(audioWait === "browser" ? "Enable audio before transmitting." : "Audio is not connected yet."); return; }
    if (!c.mic?.getAudioTracks().some(track => track.readyState === "live")) { deviceResult("Choose a working microphone before transmitting."); return; }
    if (c.held || announcement) return;
    c.held = true; c.requestID = c.latestRequestID = crypto.randomUUID(); c.track.enabled = false;
    send(c, {type: "ptt_start", request_id: c.requestID, talkgroups: targets(), secure: node("test-secure").checked, ...transmitKey()});
    c.renew = setInterval(() => { if (c.held) send(c, {type: "ptt_renew", request_id: c.requestID}); }, 250);
    node("test-transmit").textContent = "Requesting channel";
  }
  function tone(key) {
    const c = connection, keys = ["123", "456", "789", "*0#"]; stopTone(c);
    if (!c?.held || !c.track?.enabled) return;
    const row = keys.findIndex(value => value.includes(key)), column = keys[row].indexOf(key);
    c.tone = [[697,770,852,941][row], [1209,1336,1477][column]].map(frequency => {
      const oscillator = c.audio.createOscillator(), gain = c.audio.createGain(); gain.gain.value = 0.12; oscillator.frequency.value = frequency;
      oscillator.connect(gain); gain.connect(c.mix); gain.connect(c.recordMix); oscillator.start(); oscillator.onended = () => gain.disconnect(); return oscillator;
    });
  }
  function closeReceive(receive) {
    receive.player.pause(); receive.player.srcObject = null; receive.player.remove();
    [receive.source, receive.gain, receive.pan].forEach(part => part.disconnect());
  }
  function disconnect(c = connection) {
    if (!c) return;
    if (connection === c) connection = null;
    release(c); clearInterval(c.meter); clearTimeout(c.connectTimer); clearTimeout(c.resumeTimer);
    c.rejectConnect?.(new Error("Audio connection cancelled.")); c.rejectResume?.(new Error("Audio connection cancelled.")); c.controller.abort();
    if (recording) stopRecording();
    if (c.socket) { c.socket.onopen = c.socket.onclose = c.socket.onerror = c.socket.onmessage = null; c.socket.close(); }
    if (c.peer) { c.peer.onconnectionstatechange = c.peer.ontrack = c.peer.onicecandidate = null; c.peer.close(); }
    stopMicrophone(c); c.track?.stop(); c.receive.forEach(closeReceive); c.receive.clear();
    if (c.player) { c.player.pause(); c.player.srcObject = null; c.player.remove(); }
    if (c.audio) { c.audio.onstatechange = null; c.audio.close().catch(() => {}); }
    node("test-connection").textContent = "Disconnected"; node("test-transmit").textContent = "Idle"; node("test-mic-level").value = node("test-rx-level").value = 0;
  }
  function audioFailed(c, error) {
    if (c !== connection) return;
    disconnect(c);
    audioWait = error.name === "NotAllowedError" ? "browser" : [401, 403].includes(error.status) ? "account" : ["NotFoundError", "OverconstrainedError", "NotSupportedError", "SecurityError"].includes(error.name) ? "device" : "";
    node("test-connect").hidden = audioWait !== "browser";
    if (audioWait) {
      node("test-connection").textContent = audioWait === "browser" ? "Enable audio" : "Audio unavailable";
      result(error.message + (audioWait === "browser" ? " Allow audio in this browser, then choose Enable audio." : ""));
      return;
    }
    if (!audioWanted) return;
    if (!navigator.onLine) { node("test-connection").textContent = "Waiting for network"; result(error.message); return; }
    const delay = retryDelay; retryDelay = Math.min(retryDelay * 2, 30000);
    node("test-connection").textContent = `Reconnecting in ${delay / 1000}s`;
    result(error.message);
    clearTimeout(retryTimer); retryTimer = setTimeout(() => connect(), delay);
  }
  async function resumeAudio(c) {
    try {
      await new Promise((resolve, reject) => {
        c.rejectResume = reject;
        c.resumeTimer = setTimeout(() => reject(new DOMException("The browser paused audio playback.", "NotAllowedError")), 1000);
        (c.resume || c.audio.resume()).then(resolve, reject);
      });
    } finally { clearTimeout(c.resumeTimer); c.rejectResume = null; }
  }
  function receiveTrack(c, event) {
    if (c !== connection) return;
    let stream = event.streams?.[0];
    const receiverIndex = c.peer.getTransceivers().filter(value => value.direction === "recvonly").indexOf(event.transceiver);
    const id = Number(/^tg-(\d+)$/.exec(stream?.id || "")?.[1]) || c.ticket.receive_groups[receiverIndex];
    if (!node("listen-" + id)) return;
    stream ||= new MediaStream([event.track]);
    if (c.receive.has(id)) closeReceive(c.receive.get(id));
    const source = c.audio.createMediaStreamSource(stream), gain = c.audio.createGain(), pan = c.audio.createStereoPanner();
    gain.gain.value = 0; source.connect(gain); gain.connect(pan); pan.connect(c.playback); pan.connect(c.recordMix); pan.connect(c.rxMeter);
    const player = document.createElement("audio"); player.hidden = true; player.muted = true; player.autoplay = true; player.setAttribute("playsinline", ""); player.srcObject = stream; document.body.appendChild(player);
    const receive = {source, gain, pan, player, admitted: false}; c.receive.set(id, receive); applyReceive(c);
    Promise.resolve(player.play()).catch(error => audioFailed(c, error));
    event.track.onended = () => { if (c.receive.get(id) === receive) { closeReceive(receive); c.receive.delete(id); c.calls.delete(id); applyReceive(c); } };
  }
  async function connect(userGesture = false) {
    if (!audioWanted || connection || (audioWait && !userGesture)) return;
    clearTimeout(retryTimer); audioWait = ""; node("test-connect").hidden = true;
    const c = {receive: new Map(), calls: new Map(), localICE: [], remoteICE: [], held: false, requestID: "", latestRequestID: "", controller: new AbortController()};
    connection = c; node("test-connection").textContent = "Connecting";
    try {
      if (window.AudioContext) c.audio = new AudioContext();
      // A fallback click must resume synchronously within its user activation.
      if (userGesture && c.audio) { c.resume = c.audio.resume(); c.resume.catch(() => {}); }
      c.connectTimer = setTimeout(() => audioFailed(c, new Error("Loading the audio account timed out.")), 10000);
      if (!accountReady) await reload(c.controller.signal);
      clearTimeout(c.connectTimer); if (c !== connection) return;
      if (!navigator.mediaDevices?.getUserMedia || !c.audio) throw new DOMException("Browser audio requires HTTPS and microphone support.", "NotSupportedError");
      c.mix = c.audio.createMediaStreamDestination(); c.recordMix = c.audio.createMediaStreamDestination(); c.playback = c.audio.createMediaStreamDestination();
      c.recordTX = c.audio.createGain(); c.recordTX.gain.value = 0; c.recordTX.connect(c.recordMix);
      c.micMeter = c.audio.createAnalyser(); c.rxMeter = c.audio.createAnalyser();
      c.player = document.createElement("audio"); c.player.hidden = true; c.player.muted = true; c.player.dataset.output = "receive"; c.player.srcObject = c.playback.stream; c.player.setAttribute("playsinline", ""); document.body.appendChild(c.player);
      await useMicrophone(c); if (c !== connection) return;
      // Active capture permits autoplay in browsers that support that exception.
      await resumeAudio(c); if (c !== connection) return;
      c.audio.onstatechange = () => {
        if (c.peer?.connectionState === "connected" && ["suspended", "interrupted"].includes(c.audio.state)) audioFailed(c, new DOMException("The browser paused audio playback.", "NotAllowedError"));
      };
      await useSpeaker(); if (c !== connection) return;
      await listDevices(); if (c !== connection) return;
      c.connectTimer = setTimeout(() => audioFailed(c, new Error("Audio connection timed out.")), 10000);
      c.ticket = await request("/api/dispatch/session", "POST", {}, c.controller.signal);
      if (c !== connection) return;
      loadKeys(c.ticket.keys);
      c.peer = new RTCPeerConnection({iceServers: []});
      c.track = c.mix.stream.getAudioTracks()[0]; c.track.enabled = false;
      const sender = c.peer.addTransceiver(c.track, {direction: "sendonly", streams: [new MediaStream([c.track])]});
      const codecs = RTCRtpSender.getCapabilities("audio").codecs.filter(codec => codec.mimeType.toLowerCase() === "audio/pcmu");
      if (sender.setCodecPreferences && codecs.length) sender.setCodecPreferences(codecs);
      for (const id of c.ticket.receive_groups) {
        const receiver = c.peer.addTransceiver("audio", {direction: "recvonly"});
        if (receiver.setCodecPreferences && codecs.length) receiver.setCodecPreferences(codecs);
      }
      c.peer.ontrack = event => receiveTrack(c, event);
      c.peer.onicecandidate = event => {
        if (!event.candidate || c !== connection) return;
        const candidate = event.candidate.toJSON();
        if (c.offered) send(c, {type: "ice", candidate}); else c.localICE.push(candidate);
      };
      c.peer.onconnectionstatechange = () => {
        if (c !== connection) return;
        node("test-connection").textContent = c.peer.connectionState;
        if (c.peer.connectionState === "connected") {
          if (c.audio.state !== "running") { audioFailed(c, new DOMException("The browser paused audio playback.", "NotAllowedError")); return; }
          clearTimeout(c.connectTimer); retryDelay = 1000;
        }
        if (["failed", "closed", "disconnected"].includes(c.peer.connectionState)) audioFailed(c, new Error("Audio connection ended."));
      };
      const address = new URL(c.ticket.socket_path || "/api/dispatch/audio", location.href);
      address.protocol = address.protocol === "https:" ? "wss:" : "ws:"; address.searchParams.set("ticket", c.ticket.ticket);
      c.socket = new WebSocket(address.href); c.socket.onmessage = event => message(c, event);
      await new Promise((resolve, reject) => {
        c.rejectConnect = reject;
        c.socket.onopen = resolve;
        c.socket.onerror = () => { const error = new Error("Audio connection failed."); reject(error); audioFailed(c, error); };
        c.socket.onclose = () => { const error = new Error("Audio connection closed."); reject(error); audioFailed(c, error); };
      });
      if (c !== connection) return;
      const offer = await c.peer.createOffer(); if (c !== connection) return;
      await c.peer.setLocalDescription(offer); if (c !== connection) return;
      send(c, {type: "offer", sdp: offer.sdp}); c.offered = true;
      c.localICE.forEach(candidate => send(c, {type: "ice", candidate})); c.localICE = []; subscriptions();
      c.meter = setInterval(() => {
        if (c !== connection) return;
        for (const [meter, id] of [[c.micMeter, "test-mic-level"], [c.rxMeter, "test-rx-level"]]) {
          const samples = new Float32Array(meter.fftSize); meter.getFloatTimeDomainData(samples);
          node(id).value = 100 * Math.sqrt(samples.reduce((sum, value) => sum + value * value, 0) / samples.length);
        }
      }, 100);
    } catch (error) { audioFailed(c, error); }
  }
  async function message(c, event) {
    if (c !== connection) return;
    try {
      const value = JSON.parse(event.data);
      if (value.type === "answer") {
        await c.peer.setRemoteDescription({type: "answer", sdp: value.sdp}); if (c !== connection) return;
        for (const candidate of c.remoteICE) await c.peer.addIceCandidate(candidate); c.remoteICE = [];
      } else if (value.type === "ice" && value.candidate) {
        if (c.peer.remoteDescription) await c.peer.addIceCandidate(value.candidate); else c.remoteICE.push(value.candidate);
      } else if (value.type === "state") {
        const current = value.state || value, tx = current.tx || {};
        const admitted = c.held && !!c.requestID && tx.state === "transmitting" && tx.request_id === c.requestID;
        c.track.enabled = admitted; c.recordTX.gain.value = admitted ? 1 : 0;
        node("test-transmit").textContent = admitted ? (tx.secure ? `Transmitting encrypted · Algorithm ${tx.algorithm_id}, Key ${tx.key_id}` : "Transmitting clear") : c.held ? tx.state || "Requesting channel" : "Idle";
        if (current.ended_request_id && current.ended_request_id === c.requestID) release(c);
        log("Call state: " + JSON.stringify({transmit: tx, ended_request_id: current.ended_request_id, available: current.available, floors: current.floors}));
      } else if (value.type === "error") {
        if (value.action?.startsWith("ptt_") && value.request_id !== c.latestRequestID) return;
        if (!value.request_id || value.request_id === c.requestID) release(c);
        result(value.message || "Operation failed.");
      } else if (value.type === "rx") {
        const old = c.calls.get(value.talkgroup_id);
        if (value.active) c.calls.set(value.talkgroup_id, value);
        else if (old && (!value.call_id || value.call_id === old.call_id)) c.calls.delete(value.talkgroup_id);
        applyReceive(c); log("Received call: " + JSON.stringify(value));
      }
    } catch (error) { if (c === connection) { release(c); result(error.message); } }
  }
  async function reload(signal) {
    release();
    const next = await request("/api/state", "GET", undefined, signal); csrf = next.csrf || csrf; state = next;
    const messages = node("messages");
    if (messages && window.TrunkMessaging) {
      if (messaging) messaging.setUser(next.user?.id);
      else messaging = window.TrunkMessaging.mount(messages, {request, userID: next.user?.id});
    }
    groups = state.system?.talkgroups || []; zones = state.profile?.zones || [];
    node("test-identity").textContent = `${state.username || ""} · Radio ID ${state.user?.radio_id || "not assigned"}${state.is_administrator ? " · Administrator account" : ""}`;
    if (!node("test-group").value) node("test-group").value = String(state.profile?.selected_talkgroup || groups[0]?.tgid || "");
    node("test-groups").innerHTML = groups.map(group => `<option value="${escape(group.tgid)}">${escape(group.alias)}</option>`).join("");
    if (ptt) node("test-zone").innerHTML = zones.map((zone, index) => `<option value="${index}">${escape(zone.name)}</option>`).join("");
    const settings = new Map(groups.map(group => {
      const existing = node("listen-" + group.tgid);
      return [group.tgid, existing ? {muted: !existing.checked, volume: Number(node("volume-" + group.tgid).value) / 100, pan: node("pan-" + group.tgid).value} : state.profile?.receive?.find(value => value.talkgroup_id === group.tgid) || {}];
    }));
    node("test-receive").innerHTML = groups.map(group => {
      const setting = settings.get(group.tgid);
      return `<tr><td><input id="listen-${group.tgid}" type="checkbox" ${setting.muted ? "" : "checked"} aria-label="Listen to ${escape(group.alias)}"></td>
        <td>${escape(group.alias)} (${escape(group.tgid)})</td><td><input id="volume-${group.tgid}" type="range" min="0" max="100" step="any" value="${escape(100 * (setting.volume ?? 1))}" aria-label="Volume (%) for ${escape(group.alias)}"><output for="volume-${group.tgid}"></output></td>
        <td><select id="pan-${group.tgid}" aria-label="Speaker for ${escape(group.alias)}">${[[-1,"Left"],[0,"Both"],[1,"Right"]].map(([value,name]) => `<option value="${value}" ${Number(setting.pan || 0) === value ? "selected" : ""}>${name}</option>`).join("")}</select></td><td id="call-${group.tgid}">Idle</td></tr>`;
    }).join("");
    for (const input of node("test-receive").querySelectorAll("input,select")) {
      const display = () => { if (input.type === "range") input.nextElementSibling.value = input.value + "%"; };
      display(); input.oninput = () => { display(); subscriptions(); };
    }
    accountReady = true; subscriptions(); log("Account and groups loaded.");
  }
  async function operator(topic, body) {
    const snapshot = await request("/api/operator/" + topic, "POST", body);
    node("test-controller-results").textContent = readable(snapshot); result("Controller response received. Read the recorded outcome below."); return snapshot;
  }
  function action(id, operation, feedback = result) { node(id).onclick = async () => { try { await operation(); } catch (error) { feedback(error.message); } }; }
  async function fileSend(path, body) {
    if (announcement) return;
    release(); const controller = new AbortController(); announcement = controller;
    try { result(readable(await request(path, "POST", body, controller.signal))); }
    catch (error) { result(error.name === "AbortError" ? "Announcement cancelled." : error.message); }
    finally { if (announcement === controller) announcement = null; }
  }
  function stopRecording() { if (recording && recording.state !== "inactive") recording.stop(); }
  function retryAudio() {
    if (!audioWanted || connection || audioWait === "browser") return;
    audioWait = ""; connect();
  }
  function deviceError(error) {
    if (connection && error.name === "NotAllowedError") audioFailed(connection, error);
    deviceResult(error.message);
  }
  action("test-refresh", async () => { await reload(); retryAudio(); });
  action("test-connect", () => { disconnect(); audioWait = ""; return connect(true); });
  action("test-devices", async () => {
    let probe;
    try {
      if (!connection?.mic) probe = await navigator.mediaDevices.getUserMedia({audio: true, video: false});
      await listDevices();
      retryAudio();
    } finally { probe?.getTracks().forEach(track => track.stop()); }
  }, deviceResult);
  node("test-microphone").onchange = async () => {
    saveDevices();
    try { if (connection) await useMicrophone(connection); else retryAudio(); }
    catch (error) { deviceError(error); }
  };
  node("test-speaker").onchange = async () => { saveDevices(); try { if (connection) await useSpeaker(); else retryAudio(); } catch (error) { deviceError(error); } };
  action("test-speaker-resume", async () => { await useSpeaker(); retryAudio(); }, deviceResult);
  action("test-speaker-choose", async () => {
    if (!navigator.mediaDevices.selectAudioOutput) throw new Error("This browser has no speaker chooser. Use the listed devices or Browser default.");
    const device = await navigator.mediaDevices.selectAudioOutput({deviceId: node("test-speaker").value});
    node("test-speaker").add(new Option(device.label || "Chosen speaker", device.deviceId)); node("test-speaker").value = device.deviceId; saveDevices(); await useSpeaker();
  }, deviceResult);
  navigator.mediaDevices?.addEventListener("devicechange", async () => {
    try {
      await listDevices();
      if (connection && !connection.mic && audioWanted) await useMicrophone(connection);
      else if (audioWait === "device") retryAudio();
    } catch (error) { deviceError(error); }
  });
  node("test-release").onclick = () => release();
  node("test-ptt").onpointerdown = event => { if (event.button !== 0) return; event.preventDefault(); event.currentTarget.setPointerCapture(event.pointerId); startPTT(); };
  node("test-ptt").onpointerup = node("test-ptt").onpointercancel = node("test-ptt").onlostpointercapture = () => release();
  for (const button of page.querySelectorAll("[data-tone]")) {
    button.onpointerdown = event => { event.preventDefault(); event.currentTarget.setPointerCapture(event.pointerId); tone(button.dataset.tone); };
    button.onpointerup = button.onpointercancel = button.onlostpointercapture = () => stopTone();
  }
  node("test-group").onchange = () => { release(); connection?.receive.forEach(receive => { receive.admitted = false; }); subscriptions(); };
  node("test-secure").onchange = () => release();
  node("test-key").onchange = () => release();
  if (!ptt) node("test-extra-groups").onchange = () => release();
  if (ptt) {
    action("test-save-preferences", async () => {
      const receive = (state.profile?.receive || []).filter(setting => !node("listen-" + setting.talkgroup_id));
      for (const {tgid} of groups) receive.push({talkgroup_id: tgid, muted: !node("listen-" + tgid).checked, volume: Number(node("volume-" + tgid).value) / 100, pan: Number(node("pan-" + tgid).value)});
      const profile = {...state.profile, selected_talkgroup: selected(), receive};
      await request("/api/profile", "PUT", profile);
      state.profile = profile;
      result("Preferences saved.");
    });
    node("test-scan").onchange = subscriptions;
    node("test-zone").onchange = () => { release(); const zone = zones[Number(node("test-zone").value)]; node("test-groups").innerHTML = (zone?.talkgroups || []).map(id => `<option value="${escape(id)}">${escape(groups.find(group => group.tgid === id)?.alias || id)}</option>`).join(""); };
  }
  document.addEventListener("keydown", event => { if (event.code === "Space" && !event.repeat && !event.target.closest("#messages") && (!/INPUT|TEXTAREA|SELECT|BUTTON/.test(event.target.tagName) || event.target.id === "test-ptt")) { event.preventDefault(); startPTT(); } });
  document.addEventListener("keyup", event => { if (event.code === "Space" && connection?.held) { event.preventDefault(); release(); } });
  window.addEventListener("blur", () => release());
  document.addEventListener("visibilitychange", () => { if (document.hidden) release(); });
  window.addEventListener("pagehide", () => { audioWanted = false; clearTimeout(retryTimer); announcement?.abort(); disconnect(); if (recordingURL) URL.revokeObjectURL(recordingURL); });
  window.addEventListener("pageshow", event => { if (event.persisted) { audioWanted = true; retryAudio(); } });
  window.addEventListener("offline", () => { if (connection) audioFailed(connection, new Error("Network disconnected.")); });
  window.addEventListener("online", retryAudio);
  node("test-clear-log").onclick = () => { node("test-log").textContent = ""; };
  action("test-send-wav", () => {
    const file = node("test-wav").files[0]; if (!file) throw new Error("Choose a WAV file.");
    const query = new URLSearchParams({secure: String(node("test-secure").checked), ...transmitKey()}); targets().forEach(id => query.append("talkgroup_id", id));
    return fileSend("/api/dispatch/wav?" + query, file);
  });
  action("test-send-speech", () => fileSend("/api/dispatch/tts", {text: node("test-speech").value, talkgroups: targets(), secure: node("test-secure").checked, ...transmitKey()}));
  node("test-cancel-file").onclick = () => announcement?.abort();
  action("test-emergency", () => operator("emergency", {action: "declare", talkgroup_id: selected()}));
  action("test-ack", () => operator("emergency", {action: "acknowledge", id: node("test-incident").value}));
  action("test-clear", () => operator("emergency", {action: "clear", id: node("test-incident").value}));
  node("test-command").onsubmit = async event => {
    event.preventDefault();
    const fields = event.currentTarget.elements;
    try { await operator("radio-command", {action: fields.action.value, radio_id: Number(fields.radio_id.value), talkgroup_id: selected(), ...(fields.action.value === "message" ? {code: fields.code.value === "" ? null : Number(fields.code.value)} : {})}); }
    catch (error) { result(error.message); }
  };
  action("test-read-results", async () => {
    const snapshot = await request("/api/operator/state");
    node("test-controller-results").textContent = readable(snapshot);
    if (!ptt) {
      patches = snapshot.patches || [];
      node("test-patch-select").replaceChildren(new Option("Choose a patch", ""), ...patches.map(patch => new Option(`${patch.name} (${patch.id})`, patch.id)));
    }
    log("Controller results loaded.");
  });
  action("test-command-results", async () => { node("test-controller-results").textContent = readable(await request("/api/operator/radio-commands")); });
  action("test-units", async () => { node("test-controller-results").textContent = readable(await request("/api/operator/units?talkgroup_id=" + selected())); });
  action("test-history", async () => { node("test-controller-results").textContent = readable(await request("/api/history/calls?limit=100")); });
  action("test-stats", async () => {
    if (!connection?.peer) throw new Error("Audio is not connected yet.");
    const reports = await connection.peer.getStats(), values = [];
    reports.forEach(value => { if (value.type === "inbound-rtp" || value.type === "outbound-rtp") values.push(value); });
    node("test-controller-results").textContent = readable(values, true);
  });
  action("test-location", () => new Promise((resolve, reject) => navigator.geolocation.getCurrentPosition(async position => {
    try { await operator("location", {action: "report", talkgroup_id: selected(), location: {latitude: position.coords.latitude, longitude: position.coords.longitude, accuracy: position.coords.accuracy, fix_at: new Date(position.timestamp).toISOString()}}); resolve(); }
    catch (error) { reject(error); }
  }, reject)));
  if (!ptt) {
    node("test-patch-select").onchange = () => {
      const patch = patches.find(value => value.id === node("test-patch-select").value);
      if (!patch) return;
      const fields = node("test-patch").elements, members = patch.talkgroups.join(",");
      fields.name.value = patch.name;
      fields.algorithm_id.value = patch.algorithm_id;
      fields.key_id.value = patch.key_id;
      fields.talkgroups.value = members;
      node("test-patch-id").value = patch.id;
      node("test-patch-previous").value = node("test-patch-members").value = members;
    };
    node("test-patch").onsubmit = async event => {
      event.preventDefault(); const fields = event.currentTarget.elements;
      try { await operator("patch", {action: "activate", patch: {name: fields.name.value, talkgroups: numbers(fields.talkgroups.value), algorithm_id: Number(fields.algorithm_id.value), key_id: Number(fields.key_id.value)}}); }
      catch (error) { result(error.message); }
    };
    action("test-patch-end", () => operator("patch", {action: "end", id: node("test-patch-id").value}));
    action("test-patch-edit", () => operator("patch", {action: "update", id: node("test-patch-id").value, patch: {name: node("test-patch").elements.name.value, algorithm_id: Number(node("test-patch").elements.algorithm_id.value), key_id: Number(node("test-patch").elements.key_id.value), talkgroups: numbers(node("test-patch-members").value), previous_talkgroups: numbers(node("test-patch-previous").value)}}));
  }
  action("test-record", () => {
    if (!connection?.recordMix) throw new Error("Audio is not connected yet.");
    if (recording) return;
    if (recordingURL) { URL.revokeObjectURL(recordingURL); recordingURL = ""; }
    node("test-recording").textContent = "Recording test audio…";
    const chunks = []; let bytes = 0;
    const owner = recording = new MediaRecorder(connection.recordMix.stream, {audioBitsPerSecond: 32000});
    owner.ondataavailable = event => {
      if (bytes + event.data.size > 32 * 1024 * 1024) { stopRecording(); result("Recording stopped at the 32 MiB session limit."); return; }
      bytes += event.data.size; chunks.push(event.data);
    };
    owner.onerror = event => result(event.error?.message || "Recording failed.");
    owner.onstop = () => {
      recordingURL = URL.createObjectURL(new Blob(chunks, {type: owner.mimeType}));
      node("test-recording").innerHTML = `<audio controls src="${escape(recordingURL)}"></audio> <a download="radio-test.${owner.mimeType.includes('mp4') ? 'mp4' : 'webm'}" href="${escape(recordingURL)}">Download recording</a>`;
      if (recording === owner) recording = null;
      const player = node("test-recording").querySelector("audio"); player.muted = true;
      useSpeaker().catch(error => deviceResult(error.message));
    };
    owner.start(1000);
  });
  node("test-record-stop").onclick = stopRecording;
  node("test-record-delete").onclick = () => {
    if (recording) { result("Stop recording before deleting it."); return; }
    if (recordingURL) URL.revokeObjectURL(recordingURL); recordingURL = ""; node("test-recording").textContent = "";
  };
  connect();
})();
