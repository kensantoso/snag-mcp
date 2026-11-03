// Renders a Bunnings product page in headless Chrome with a store selected the
// way a shopper selects one, and prints what the page shows as JSON:
//
//   node product.mjs <product-url> <postcode> <store-name> <store-id>
//
// Output: {store, price_text, in_store_text, aisle, bay, api: {priceInfo,
// fulfillment, locations}}. api holds the data the page used for that store,
// whether it fetched it in the browser or the server rendered it into the
// page (which it does once the store is remembered). It uses only Chrome's DevTools protocol and Node's built-in
// WebSocket, so there is nothing to install. CHROME overrides the browser path.
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const [url, postcode, storeName, storeID] = process.argv.slice(2);
if (!url || !postcode || !storeName || !storeID) {
  console.error('usage: node product.mjs <product-url> <postcode> <store-name> <store-id>');
  process.exit(2);
}
const chrome = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const dir = mkdtempSync(join(tmpdir(), 'bunnings-e2e-'));
const proc = spawn(chrome, ['--headless=new', '--remote-debugging-port=0', `--user-data-dir=${dir}`,
  '--no-first-run', '--no-default-browser-check', '--window-size=1400,2200', 'about:blank'],
  { stdio: ['ignore', 'ignore', 'pipe'] });
const cleanup = () => { try { proc.kill(); } catch {} try { rmSync(dir, { recursive: true, force: true }); } catch {} };
let evaluateRef = null;
const fail = async (msg) => {
  if (evaluateRef && process.env.DEBUG) console.error(await evaluateRef(`(document.querySelector('[role=dialog]') || document.body).innerText.slice(0, 1500)`));
  console.error(msg); cleanup(); process.exit(1);
};
setTimeout(() => fail('timed out'), 120000).unref();

const wsURL = await new Promise((res, rej) => {
  let buf = '';
  proc.stderr.on('data', d => { buf += d; const m = buf.match(/DevTools listening on (ws:\S+)/); if (m) res(m[1]); });
  setTimeout(() => rej(new Error('Chrome did not start')), 20000);
}).catch(e => fail(e.message));
const port = new URL(wsURL).port;
const page = (await (await fetch(`http://127.0.0.1:${port}/json`)).json()).find(t => t.type === 'page');
const ws = new WebSocket(page.webSocketDebuggerUrl);
await new Promise(r => { ws.onopen = r; });
let seq = 0;
const pending = new Map();
const listeners = [];
ws.onmessage = e => {
  const m = JSON.parse(e.data);
  if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } else listeners.forEach(l => l(m));
};
const send = (method, params = {}) => new Promise(r => { const id = ++seq; pending.set(id, r); ws.send(JSON.stringify({ id, method, params })); });
const evaluate = async (expression) => (await send('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true })).result?.result?.value;
evaluateRef = evaluate;
const sleep = ms => new Promise(r => setTimeout(r, ms));
const waitFor = async (expression, ms = 20000) => {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(250)) if (await evaluate(expression)) return true;
  fail(`gave up waiting for ${expression}`);
};

// Capture the store-specific responses the page fetches after a store is set.
const wanted = { priceInfo: /\/priceInfo/, fulfillment: /\/fulfillment/, locations: /item-api\/locations/ };
// Each is kept only if it was asked for the chosen store; the last one wins.
const requests = new Map();
const api = {};
listeners.push(async m => {
  if (m.method === 'Network.requestWillBeSent') {
    const r = m.params.request;
    const store = r.headers.locationCode || r.headers.locationcode || (r.url.match(/locationCode=(\d+)/) || [])[1] || '';
    for (const [k, re] of Object.entries(wanted)) if (re.test(r.url)) requests.set(m.params.requestId, { k, store });
  }
  if (m.method === 'Network.loadingFinished' && requests.has(m.params.requestId)) {
    const { k, store } = requests.get(m.params.requestId);
    if (store !== storeID) return;
    const body = await send('Network.getResponseBody', { requestId: m.params.requestId });
    try { api[k] = JSON.parse(body.result.body).data; } catch {}
  }
});

await send('Page.enable');
await send('Network.enable');
await send('Network.setUserAgentOverride', { userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36' });
const storeButton = `document.querySelector('[data-locator=selectStoreButton]') || [...document.querySelectorAll('button')].find(b => /change store/i.test(b.innerText))`;
const present = async (expression, ms) => {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(250)) if (await evaluate(expression)) return true;
  return false;
};

// Picks the store through the page's own store finder, as a shopper would.
async function chooseStore() {
  await sleep(1500);
  await evaluate(`(${storeButton}).click()`);
  await waitFor(`!!document.getElementById('storeAutoSuggest-textbox')`);
  await evaluate(`(() => { const i = document.getElementById('storeAutoSuggest-textbox'); i.focus(); i.select(); })()`);
  await send('Input.insertText', { text: postcode });
  const option = `[...document.querySelectorAll('#storeAutoSuggest-listbox [role=option]')].find(o => o.innerText.includes(${JSON.stringify(postcode)}))`;
  await waitFor(`!!${option}`);
  await evaluate(`${option}.click()`);
  const tile = `[...document.querySelectorAll('[role=dialog] [data-locator=Tile_Store]')].find(b => b.innerText.includes(${JSON.stringify(storeName)}))`;
  await waitFor(`!!${tile}`);
  await sleep(1000);
  await evaluate(`${tile}.click()`);
  await waitFor(`!!document.querySelector('[role=dialog] [data-locator=Button_UseThisStore]')`);
  await evaluate(`document.querySelector('[role=dialog] [data-locator=Button_UseThisStore]').click()`);
  await waitFor(`!document.querySelector('[role=dialog] [data-locator=Button_UseThisStore]') && /Store:/.test(document.body.innerText)`);
}

await send('Page.navigate', { url });
if (await present(`!!(${storeButton})`, 40000)) {
  await chooseStore();
} else {
  // A product not ranged at the browser's default store (Bunnings picks one
  // from the IP address) renders as "no longer available", with no store
  // finder. Choose the store on a product sold everywhere, then come back.
  await send('Page.navigate', { url: 'https://www.bunnings.com.au/full-boar-25-piece-hss-m2-drill-bit-set_p0563516' });
  await waitFor(`!!(${storeButton})`, 40000);
  await chooseStore();
  for (const k of Object.keys(api)) delete api[k]; // those were the other product's
  await send('Page.navigate', { url });
  await waitFor(`/Store:/.test(document.body.innerText)`, 40000);
}
for (const end = Date.now() + 15000; Date.now() < end && Object.keys(api).length < 3; ) await sleep(250);
await sleep(1000);

// What the server rendered into the page, keyed [kind, item, store].
const ssr = await evaluate(`(() => {
  try {
    return JSON.parse(document.getElementById('__NEXT_DATA__').textContent).props.pageProps.dehydratedState.queries
      .map(q => ({ key: q.queryKey, data: q.state.data }));
  } catch { return []; }
})()`) || [];
const item = (url.match(/_p(\d+)/) || [])[1];
for (const [kind, k] of [['product-retail-price', 'priceInfo'], ['product-fulfilment', 'fulfillment'], ['product-aisle-item-location', 'locations']]) {
  const q = ssr.find(q => q.key[0] === kind && q.key[1] === item && q.key[2] === storeID);
  if (q && !api[k]) api[k] = q.data;
}
const text = await evaluate('document.body.innerText');
const after = text.slice(text.indexOf('I/N:'));
const price = after.match(/\n\s*(\$[0-9][0-9,]*(?:\.[0-9]{2})?)\s*\n/);
const store = (text.match(/Store:\s*\n\s*([^\n]+)/) || [])[1];
const inStore = (after.match(/\nIn-store\s*\n([\s\S]*?)\nDelivery/) || [])[1]?.trim().replace(/\s*\n\s*/g, ' | ');
// The page shows aisle and bay only as the store-map button's label.
const aisle = await evaluate(`[...document.querySelectorAll('button[aria-label]')].map(b => b.getAttribute('aria-label')).find(l => /store map showing/i.test(l)) || ''`);
const loc = aisle.match(/Aisle\s+(\S+)\s+Bay\s+(\S+)/i);
console.log(JSON.stringify({ store, price_text: price && price[1], in_store_text: inStore, aisle: loc ? loc[1] : '', bay: loc ? loc[2] : '', api }));
cleanup();
process.exit(0);
