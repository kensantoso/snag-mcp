# Working in this repo

snag-mcp: an MCP server for shopping at Bunnings in person, over stdio. It
calls the public APIs behind bunnings.com.au with a guest token; there is no
account, key or browser. Unofficial and not affiliated with Bunnings: keep it
that way in anything user-facing.

**No Bunnings content in the repo, and no Bunnings branding.** Bunnings' terms
reserve its copyright and trade marks, so:

- Test fixtures are invented, in the APIs' shapes: made-up item numbers,
  stores, names and text. Never commit a recorded response (products, search,
  stock, stores, maps, reviews), even trimmed. Only the opt-in live tests in
  `e2e` may name real items and stores, since they fetch at run time and store
  nothing.
- Pictures in docs come from the synthetic Example Store
  (`scripts/example-store`), never a real store's plan or a screenshot of a
  real result.
- "Bunnings" describes what the tool works with (descriptions, the
  unofficial note, the `internal/bunnings` client); it is not in names,
  titles or branding, and the map and view use no Bunnings logo or brand
  colours.

## Commands

```sh
go build ./... && go vet ./... && go test ./...
gofmt -l .                       # must print nothing; CI fails otherwise

BUNNINGS_LIVE=1 go test -v -timeout 15m ./e2e   # live, against bunnings.com.au
go run ./cmd/snag-mcp -config-path               # where the location is saved
go run ./scripts/example-store internal/storemap/testdata   # rewrite the synthetic store fixture
./scripts/update-postcodes.sh                    # refresh the embedded GeoNames data
```

Exercising a tool without a client: the server speaks JSON-RPC on stdin and
closes on EOF, so hold the pipe open.

```sh
{ echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}'
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  echo '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_products","arguments":{"query":"drill bit set","location":"3194"}}}'
  sleep 5
} | go run ./cmd/snag-mcp
```

The live tests compare against the product page rendered in headless Chrome
(`e2e/browser/product.mjs`, Node 22+, no npm packages). Without Chrome they
compare against the website's APIs only.

Seeing the MCP App after changing `internal/tools/app/pick-list.html`: build
the binary, then let the preview script play the host and screenshot it
(Chrome, python3; no npm). `--record DIR` replays an earlier run's results
without calling Bunnings, view HTML included as the binary served it then, so
drop it after changing the view.

```sh
go build -o /tmp/snag-mcp ./cmd/snag-mcp
scripts/app-preview/preview.py /tmp/snag-mcp /tmp/prev hawthorn \
  '{"store":"6299","location":"3122","items":[{"item_number":"0563516"},{"item_number":"0548292"},{"item_number":"8032174"}]}' \
  --width 380 --theme dark --click '[data-floor="1"]'
```

Inspect the PNGs; the `.log` beside each lists the messages the host saw. Those
screenshots are of real stores: keep them out of the repo.

The README's pictures and worked example come from the synthetic store, with
no network:

```sh
go run ./scripts/example-store -demo /tmp/demo        # pick-list.txt, map-example.png, example.json
scripts/app-preview/preview.py none /tmp/demo example '{}' --record /tmp/demo
```

## Layout

```
cmd/snag-mcp         flags, server startup
internal/bunnings    HTTP client for the website's APIs
internal/geo         postcode/suburb -> point, from embedded GeoNames data
internal/shop        pure rules: availability, pick list, ranking, load, brands, connections
internal/storemap    store floor plans: parse, plan the walk, draw a PNG (pure Go, no cgo)
internal/tools       the five tools, and the pick-list MCP App
internal/tools/app   the App's one HTML page, embedded; hand-written, no build step
scripts/app-preview  plays an MCP Apps host in headless Chrome, for screenshots
scripts/example-store the synthetic two-floor store: test fixture and README demo
e2e                  opt-in live tests: built binary over stdio vs the site
```

## Things that will bite

**The availability rule is the product.** An item qualifies at a store only if
`fulfilmentoptions_<store>` has In-Store or Click & Collect and
`price_<store>` > 0, and it is active and not marketplace (`sellername`, a
`Marketplace` product range, or `isMarketLink`). Never report a price for a
store that fails this: Bunnings keeps stale prices against stores that do not
range an item (a WA-only cement, $1.54 in Melbourne). `internal/shop` owns it;
keep the tests over `testdata/reference_items.json`, which reproduces those
traps with invented items, passing.

**Tool descriptions are load-bearing.** The `Description` and `jsonschema`
tags are all the model reads when choosing a tool and its arguments. Editing
them changes behaviour, and no test catches a regression.

**Status values are machine names** (`in_stock`, `short`, `special_order`,
`not_sold_here`, ...). Unknown is unknown: missing weights, sizes and stock are
reported as missing, never estimated or defaulted.

**The website's APIs have quirks that look like bugs here:**

- The guest login 401s if the authorize parameters are in alphabetical order.
  Keep the website's order.
- The multi-store stock endpoint reports special orders as `null` or the
  placeholder `9999`. Special-order status comes from product details, and
  those are store-dependent: send `locationCode`.
- Aisle and bay come from `item-api/locations`, which is what the product page
  shows. Fulfilment sometimes carries an `inStoreItemLocation` that disagrees;
  ignore it.
- `/v1/stores` ignores small radii and pads large ones; distance is measured
  here, from the same point, and the boundary is ours.
- Store floor plans come from map-viewer.apps.bunnings.com.au, behind a bot
  filter that 403s anything without a browser User-Agent. Send the viewer's
  Referer too. The plan's y already runs down the page as on the printed PDF
  (`/api/<store>/map-<store>.pdf`): do not flip it. The walking graph stores y
  positive and the viewer negates it on reading; so do we. Map aisle and bay
  are integers, `item-api/locations` gives strings: join on the numbers.
- Reviews are Bazaarvoice's, keyed with the `mprAPIKey` from its widget
  config, fetched at runtime with an embedded fallback.

**The MCP App must not change what other hosts get.** `check_list`'s result
(text, PNG, JSON) is the same for every client; the view reads it as is.
`get_store_layout` is app-only (`_meta.ui.visibility: ["app"]`), and is also
left out of `tools/list` for clients that do not declare
`io.modelcontextprotocol/ui`, read per request through
`ServerRequest.ClientCapabilities` (per-request `_meta` on the 2026-07-28
protocol, `initialize` before it). The view talks the postMessage protocol by
hand, not through the ext-apps SDK, so there is no npm or bundler in the build;
keep it one file that loads nothing (the hosts' default CSP blocks the rest),
and keep the footer saying it is unofficial. Its layout coordinates are
`check_list`'s `x_m`/`y_m` frame in decimetres; change both or neither.

**Be a good guest.** Requests are bounded by one semaphore in
`bunnings.Client` (default 4) and nothing retries except a single token
refresh on 401. Do not add retry loops or unbounded fan-out.

**Never commit a saved location or a token.** `config.json`, `*.key`,
`*.mcpb` are ignored; keep it that way.

## Commits

One commit on `main`, amended rather than added to. Author and committer are the
repo owner's GitHub noreply address. No `Co-Authored-By` lines.
