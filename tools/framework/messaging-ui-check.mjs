import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";

const [fixturePath, outputPath] = process.argv.slice(2);
assert.ok(fixturePath && outputPath, "usage: messaging-ui-check.mjs fixture.private.json output");
const fixture = JSON.parse(await fs.readFile(fixturePath, "utf8"));
const origin = new URL(fixture.origin);
assert.ok(["localhost", "127.0.0.1", "[::1]"].includes(origin.hostname), "only a disposable loopback fixture may be tested");
assert.equal(origin.pathname, "/");
assert.equal(origin.username, "");
assert.equal(origin.password, "");
assert.equal(fixture.people.length, 3);
const output = path.resolve(outputPath);
await fs.mkdir(output, {recursive: true, mode: 0o700});
const report = {
  started_utc: new Date().toISOString(),
  scope: "Real disposable Go Application Server, real Chromium, separate authenticated people, source assets; RF delivery remains pending.",
  checks: [], failures: [], network: [], screenshots: []
};
const contexts = [], pages = [], browserErrors = [];
let browser, microphoneStarts = 0;
const check = name => report.checks.push(name);
const historySelector = '#messages ol[aria-label="Messages"]';
const statusSelector = '#messages [role="status"]';
const threadButton = id => '#messages button[data-thread=' + JSON.stringify(id) + ']';
const pageBodies = page => page.locator(historySelector + " > li > p:last-child").allTextContents();
const waitBody = (page, body) => page.waitForFunction(({selector, body}) => [...document.querySelectorAll(selector + " > li > p:last-child")].some(node => node.textContent === body), {selector: historySelector, body});
const waitStatus = (page, text) => page.waitForFunction(({selector, text}) => document.querySelector(selector)?.textContent.includes(text), {selector: statusSelector, text});
const messagePath = id => "/api/messages/threads/" + encodeURIComponent(id);

async function navigate(page, route) {
  const response = await page.goto(fixture.origin + route, {waitUntil: "domcontentloaded"});
  assert.equal(response.status(), 200);
  await page.locator("#message-new").waitFor();
  await page.locator("#message-new input[type=checkbox]").first().waitFor();
  await page.locator("#test-connect").waitFor({state: "visible"});
}

async function login(person, route) {
  const context = await browser.newContext({viewport: {width: 1280, height: 1000}});
  contexts.push(context);
  await context.exposeBinding("__qaUnexpectedMicrophone", () => { microphoneStarts++; });
  await context.addInitScript(() => {
    window.qaInjected = 0;
    window.qaMicrophoneStarts = 0;
    if (navigator.mediaDevices?.getUserMedia) navigator.mediaDevices.getUserMedia = () => {
      window.qaMicrophoneStarts++; window.__qaUnexpectedMicrophone();
      return Promise.reject(new DOMException("Microphone access denied in this messaging fixture.", "NotAllowedError"));
    };
  });
  context.on("request", request => {
    const address = new URL(request.url());
    report.network.push({person: person.id, method: request.method(), path: address.pathname, same_origin: address.origin === fixture.origin});
  });
  const response = await context.request.post(fixture.origin + "/api/login", {
    headers: {Origin: fixture.origin}, data: {username: person.username, password: fixture.password}
  });
  assert.equal(response.status(), 200, "fixture person signs in");
  const signedIn = await response.json();
  assert.ok(signedIn.csrf, "real login returned a CSRF token");
  const page = await newPage(context, person.id);
  await navigate(page, route);
  await page.waitForFunction(id => document.querySelector("#messages")?.textContent.includes("(" + id + ")"), person.id);
  return {context, page};
}

async function newPage(context, person) {
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  page.on("pageerror", error => browserErrors.push({person, message: error.message}));
  pages.push(page);
  return page;
}

async function create(page, kind, members, name = "") {
  await page.locator("#message-kind").selectOption(kind);
  await page.locator("#message-name").fill(name);
  for (const id of members) await page.locator('#message-new input[value=' + JSON.stringify(id) + ']').check();
  await page.locator("#message-new").getByRole("button", {name: "Create conversation", exact: true}).click();
  await waitStatus(page, "Conversation loaded");
  const selected = page.locator('#messages button[data-thread][aria-pressed="true"]');
  await selected.waitFor();
  return selected.getAttribute("data-thread");
}

async function openThread(page, id) {
  await page.locator(threadButton(id)).waitFor();
  const loaded = page.waitForResponse(response => response.request().method() === "GET" && new URL(response.url()).pathname === messagePath(id));
  await page.locator(threadButton(id)).click();
  assert.equal((await loaded).status(), 200);
  await waitStatus(page, "Conversation loaded");
}

async function send(page, body) {
  await page.locator("#message-body").fill(body);
  await page.locator("#message-compose").getByRole("button", {name: "Send message", exact: true}).click();
  await page.waitForFunction(() => document.querySelector("#message-body").value === "");
  await waitBody(page, body);
}

async function setMember(page, userID, included) {
  const form = page.locator("#message-members");
  await form.waitFor({state: "visible"});
  await form.locator('input[value=' + JSON.stringify(userID) + ']').setChecked(included);
  await form.getByRole("button", {name: "Save members", exact: true}).click();
  await waitStatus(page, "Members saved");
}

async function screenshot(page, filename) {
  await page.locator("#messages").screenshot({path: path.join(output, filename)});
  report.screenshots.push(filename);
}

try {
  const module = process.env.TRUNKSYSTEM_PLAYWRIGHT || 'playwright';
  const {chromium} = await import(module);
  browser = await chromium.launch({headless: true, channel: "chrome"});
  const [deskPerson, pttPerson, outsiderPerson] = fixture.people;
  const desk = await login(deskPerson, "/dispatch-test");
  const ptt = await login(pttPerson, "/ptt-test");
  const outsider = await login(outsiderPerson, "/ptt-test");
  check("Separate real browser sessions sign in to Dispatch and PTT Testing.");

  const directID = await create(desk.page, "direct", [pttPerson.id]);
  const original = "  Typed in Dispatch: Café e\u0301 / 東京 🙂\n\n<script>window.qaInjected = 1</script>\n  final line  ";
  await send(desk.page, original);
  // No Refresh is clicked in the recipient: appearance of the thread comes
  // through the real authenticated EventSource connection.
  await ptt.page.locator(threadButton(directID)).waitFor();
  await openThread(ptt.page, directID);
  await waitBody(ptt.page, original);
  assert.deepEqual(await pageBodies(ptt.page), [original]);
  assert.equal(await ptt.page.evaluate(() => window.qaInjected), 0);
  check("Dispatch typed Unicode/newlines arrive in the PTT inbox through SSE and render as plain text.");
  const reply = "Reply from PTT\nwith its original line break.";
  await send(ptt.page, reply);
  await waitBody(desk.page, reply);
  check("PTT replies arrive live in the already open Dispatch conversation.");

  const sharedPage = await newPage(desk.context, deskPerson.id);
  await navigate(sharedPage, "/ptt-test");
  await openThread(sharedPage, directID);
  assert.deepEqual(await pageBodies(sharedPage), [original, reply]);
  check("The same person sees one shared inbox from a new PTT page and the Dispatch app.");
  await screenshot(desk.page, "dispatch-direct-conversation.png");

  const groupID = await create(desk.page, "group", [pttPerson.id], "Browser field team");
  const secret = "Initial members only: this predates the later member.";
  await send(desk.page, secret);
  assert.equal(await outsider.page.locator(threadButton(groupID)).count(), 0);
  await setMember(desk.page, outsiderPerson.id, true);
  await outsider.page.locator(threadButton(groupID)).waitFor();
  await openThread(outsider.page, groupID);
  assert.deepEqual(await pageBodies(outsider.page), []);
  assert.ok(!(await outsider.page.locator("#messages").textContent()).includes(secret));
  const visible = "Added member can see this later message.\nActual group delivery.";
  await send(desk.page, visible);
  await waitBody(outsider.page, visible);
  assert.deepEqual(await pageBodies(outsider.page), [visible]);
  check("Creator adds a private-group member; earlier history stays hidden and later text arrives live.");
  await screenshot(outsider.page, "group-new-member-history.png");

  await setMember(desk.page, outsiderPerson.id, false);
  await outsider.page.waitForFunction(({id, selector}) => !document.querySelector('#messages button[data-thread="' + id + '"]') && document.querySelector(selector).children.length === 0 && document.querySelector('#messages section[aria-label="Selected conversation"]').hidden, {id: groupID, selector: historySelector});
  assert.ok(!(await outsider.page.locator("#messages").textContent()).includes(visible));
  await setMember(desk.page, outsiderPerson.id, true);
  await openThread(outsider.page, groupID);
  assert.deepEqual(await pageBodies(outsider.page), []);
  check("Removing a member clears rendered history; rejoining starts with empty history.");

  await openThread(desk.page, directID);
  const retryBody = "Keep this draft after a lost HTTP receipt.\nRetry stores exactly one message.";
  const sends = [];
  const capture = request => {
    if (request.method() === "POST" && new URL(request.url()).pathname === messagePath(directID) + "/messages") sends.push(request.postDataJSON());
  };
  desk.page.on("request", capture);
  let lostReceiptID = 0, interceptions = 0;
  const pattern = "**/api/messages/threads/*/messages";
  const loseReceipt = async route => {
    if (route.request().method() !== "POST") return route.continue();
    interceptions++;
    const response = await route.fetch();
    assert.ok([200, 201].includes(response.status()), "lost-receipt request committed at the real server");
    lostReceiptID = (await response.json()).message.id;
    await route.abort("failed");
  };
  await desk.page.route(pattern, loseReceipt);
  await desk.page.locator("#message-body").fill(retryBody);
  await desk.page.locator("#message-compose").getByRole("button", {name: "Send message", exact: true}).click();
  await waitStatus(desk.page, "Your draft is kept");
  assert.equal(await desk.page.locator("#message-body").inputValue(), retryBody);
  assert.equal(interceptions, 1);
  await desk.page.unroute(pattern, loseReceipt);
  await desk.page.getByRole("button", {name: "Refresh messages", exact: true}).click();
  await waitStatus(desk.page, "Messages refreshed");
  assert.equal(sends.length, 1, "Refresh does not replay the failed send");
  assert.equal(await desk.page.locator("#message-body").inputValue(), retryBody);
  await desk.page.locator("#message-compose").getByRole("button", {name: "Send message", exact: true}).click();
  await desk.page.waitForFunction(() => document.querySelector("#message-body").value === "");
  assert.equal(sends.length, 2);
  assert.equal(sends[0].client_id, sends[1].client_id, "an explicit retry keeps the original send identifier");
  const stored = await desk.context.request.get(fixture.origin + messagePath(directID));
  assert.equal(stored.status(), 200);
  const matching = (await stored.json()).messages.filter(message => message.body === retryBody);
  assert.equal(matching.length, 1);
  assert.equal(matching[0].id, lostReceiptID);
  await desk.page.reload({waitUntil: "domcontentloaded"});
  await desk.page.locator(threadButton(directID)).waitFor();
  assert.equal(sends.length, 2, "page reload does not replay messages");
  desk.page.off("request", capture);
  check("A real committed send with an aborted receipt retains its draft; explicit retry deduplicates and refresh/reload never sends it automatically.");

  await sharedPage.setViewportSize({width: 320, height: 900});
  await sharedPage.getByRole("button", {name: "Refresh messages", exact: true}).click();
  await waitStatus(sharedPage, "Messages refreshed");
  const narrow = await sharedPage.locator("#messages").evaluate(node => ({width: node.clientWidth, scroll: node.scrollWidth}));
  assert.ok(narrow.scroll <= narrow.width, "the Messages section fits a 320px viewport");
  await screenshot(sharedPage, "ptt-messages-320px.png");
  check("Messages fit a 320px viewport.");

  for (const page of pages) {
    assert.equal(await page.locator('details.help,[data-help],.i-button,[aria-label="Information"]').count(), 0);
    assert.equal(await page.evaluate(() => window.qaInjected), 0);
    assert.equal(await page.evaluate(() => window.qaMicrophoneStarts), 1, "only the initial app audio attempt occurs; messaging never retries denied audio");
  }
  assert.ok(microphoneStarts >= pages.length, "the existing app audio startup is exercised with denied capture");
  assert.deepEqual(browserErrors, [], "browser execution has no uncaught errors");
  assert.ok(report.network.every(request => request.same_origin), "browser requests remain on the serving Application Server");
  assert.deepEqual(report.network.filter(request => !["GET", "HEAD", "OPTIONS"].includes(request.method) && request.path !== "/api/login" && !request.path.startsWith("/api/messages/")), [], "messaging never issues other app or radio mutations");
  assert.deepEqual(report.network.filter(request => /\/(?:api\/(?:dispatch|operator)|p25|internal)\//.test(request.path)), [], "messaging never connects audio, PTT or native/controller operations");
  check("App audio startup waits after denied capture; messaging causes no further capture attempts, radio/audio requests, cross-service browser requests or administrator help controls.");
} catch (error) {
  report.failures.push({message: error.message, stack: error.stack});
  process.exitCode = 1;
} finally {
  await Promise.all(contexts.map(context => context.close().catch(() => {})));
  await browser?.close();
  report.finished_utc = new Date().toISOString();
  await fs.writeFile(path.join(output, "report.json"), JSON.stringify(report, null, 2) + "\n", {mode: 0o600});
  console.log(JSON.stringify({checks: report.checks.length, failures: report.failures.length, report: path.join(output, "report.json")}));
}
