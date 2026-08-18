# Handoff: Minecraft Map Viewer — Real Texture Rendering

## Project

`C:\Users\Daniel\Downloads\mapviewer` — a Go backend + TypeScript/OpenLayers frontend that
renders a Minecraft world (Anvil region files) into a tile-pyramid web map, in both top-down
and isometric modes. Not a git repo (no version control currently in use for this project).

Backend: `backend/` (Go). Frontend: `frontend/` (Vite + OpenLayers, already built at
`frontend/dist`). Config: `config/config.example.yml`, `config/blocks.json`.

## What this session did, in order

1. Fixed 21 general codebase bugs/issues across the Go backend and TS frontend (WebSocket
   races, scheduler bugs, region-file races, various frontend state bugs). All complete,
   not relevant to what follows.
2. Added chunk selection (claim / unclaim / force-load) as a map feature. Complete.
3. **Built a full real-texture rendering pipeline** — the bulk of this session:
   - Reads textures directly from the user's real Minecraft client jar + mod jars (not
     bundled assets), so modded blocks/textures render correctly. Source paths are
     `C:\Users\Daniel\curseforge\minecraft\Install\versions\1.21.1\1.21.1.jar` and
     `C:\Users\Daniel\curseforge\minecraft\Instances\ff\mods` (175 mods).
   - Full fidelity: native 16px/block resolution (not downsampled), via a raised tile base
     zoom.
   - New package `backend/internal/textures/`: `source.go` (layered jar/zip reading via
     `archive/zip`), `model.go` (blockstate → model → texture resolution, mirroring
     Minecraft's own resolution algorithm), `atlas.go` (PNG decode + mip chain).
   - Wired into `config/config.go` (`Textures` config section), `backend/internal/app/app.go`
     (loads texture sources at startup), `backend/internal/render/shader.go` (per-texel
     sampling via `Shader.FaceSampler`), `backend/internal/render/topdown.go` and `iso.go`
     (rewritten to sample real texels instead of one flat colour per block).
   - Verified end-to-end against the real modpack; user confirmed it's genuinely reading
     their real world/textures, not fabricated.

4. **Bug: grass/fern/saplings render as floating "black hole" cubes in isometric mode.**
   Root cause and fix evolved over three iterations — read this carefully before touching
   `blocks.json` or the world scanner again:

   - **Iteration 1 (wrong):** Marked short_grass/fern/flowers/etc. `"transparent": true` in
     `blocks.json` so the world surface scanner skipped them entirely, revealing the ground
     below. User corrected this: they want the plants **visible**, not hidden — "isometric
     view should be a reliable copy."
   - **Iteration 2 (texture resolution):** Minecraft's `block/cross` / `block/tinted_cross` /
     `block/crop` models (2 or 4 crossed zero-thickness planes, no up/down face — grass,
     ferns, saplings, flowers, crops) weren't being resolved to any texture at all (the
     resolver only handled full-cube models). Added detection + a `crossFace()` extractor in
     `textures/model.go` so these get real textures. This alone reintroduced the "black hole"
     bug once combined with `transparent:true` removal, because the world scanner then
     treated e.g. a poppy as its own **solid one-block-tall surface cube** at its own height
     (one block above the grass below it) — and the poppy texture is 90% transparent, so most
     of that cube's faces render as nothing, punching a black hole with a visible height step
     versus the surrounding grass.
   - **Iteration 3 (the actual fix — "decoration" overlay architecture):** A plant is not its
     own surface; it's a **decal on top of the block below it**. Added a whole new concept
     end-to-end:
     - `blocks.Block.Decoration bool` (+ `blockJSON.Decoration` / `"decoration"` key) —
       `backend/internal/blocks/registry.go`.
     - `world.ChunkSurface.Decoration[]` / `world.Column.Decoration` / `world.Surface`
       backing array — `backend/internal/world/surface.go`. Plumbed through `At`/`Set`/`Blit`.
     - World scanner (`backend/internal/mcworld/chunk.go`): `palEntry.decoration`; the
       bulk-skip `allTransparent` optimization was widened to `!pe.transparent || pe.decoration`
       so a decoration sitting in an otherwise-all-air section doesn't get skipped past
       without being recorded (this was a real bug caught during implementation — the
       original allTransparent check would have silently dropped every decoration). The scan
       tracks the closest-to-ground decoration seen while descending and stamps it onto the
       solid block's column entry.
     - `render/shader.go`: `baseColor()` = the surface block's own colour (never a
       decoration); `topColor()` = `baseColor()` with the decoration alpha-composited over
       it, used by `ColumnColor` (whole-column colour) and — critically — by `FaceSampler`
       only for `FaceKindTop`, never `FaceKindSide` (a plant doesn't wrap the block's side
       skirt). Refactored `faceColor`/`tintColumn` to take an explicit block id so the same
       tinting code serves both the surface block and a decoration.
     - Result verified visually (screenshots) in both top-down and isometric: flowers/grass
       sit flat on real grass texture, no floating cubes, no black holes, no height-step
       artifacts.

5. **Follow-up ask (the actual reason for this handoff): "there are more blocks like this,
   e.g. Toadstool, Fall Dragon Reeds — find all of them, and consider blocks with a totally
   different 3D model."** The concern: hand-listing plant names in `blocks.json` cannot scale
   to 175 mods.

   - Investigated: confirmed via the real jar/mod data that `biomesoplenty:toadstool` and
     `spawn:fall_dragon_reeds` both actually use vanilla's `block/cross` parent already — the
     texture *resolution* already worked for them. The actual gap was that they weren't in
     `blocks.json`, so the *world scanner* didn't know to mark them `Transparent`+`Decoration`
     — same black-hole bug, just for unlisted names.
   - **Replaced the name-based check with a pure geometry-based classifier.** In
     `textures/model.go`:
     - `isBillboardShape(elements)` / `isBillboardPlane()`: a model qualifies if it has 2 or 4
       elements, each a *zero-thickness* quad (exactly one of X/Z has `from==to`), spanning
       (near-)full block height (`to.Y - from.Y >= 14`), with no `up`/`down` face. This is the
       literal geometric definition of Minecraft's cross/crop billboard shape — it does not
       care what the model file is named or what it inherits from.
     - Verified this catches vanilla AND arbitrary custom-named mod plants
       (`biomesoplenty:dead_branch`, `spawn:broad_grass`, etc. — found via a scripted scan of
       all 175 mod jars for non-`cross`/`crop`-parented models with this exact shape).
     - Verified it correctly does **not** misfire on structural cross-shaped things (iron
       bars, fences, chains-as-multipart, gold bars posts) — those are built via `multipart`
       blockstates, which this resolver already declines to touch (`len(bs.Variants) == 0` →
       reject before geometry is ever checked), so they never reach the shape check at all.
     - **One accepted, documented false positive: `minecraft:chain`.** Its model is
       *genuinely, geometrically* two crossed zero-thickness planes at full height with no
       up/down face — identical in shape to a flower cross (it just gets there via a runtime
       45° rotation instead of pre-rotated coordinates). Tried excluding elements with a
       `rotation` field, or requiring `rotation.rescale == true` — **both broke real plants**:
       vanilla's own `block/cross` uses `rotation` + `rescale:true`, and modded
       `biomesoplenty:dead_branch` / `spawn:broad_grass` use `rotation` *without* `rescale` and
       are genuine flora. There is no reliable field in the model format that distinguishes
       "chain" from "flower cross." Accepted as a narrow, low-impact known limitation (chains
       rarely form the top-of-column "surface" in a typical build) rather than adding
       complexity that doesn't actually work. This is documented in a code comment right on
       `isBillboardPlane`.
   - **Wired classification to be fully automatic, no blocks.json maintenance needed:**
     - `textures.ClassifyDecoration(store, blockID) bool` — exported, shares blockstate/chain
       loading with `resolveBlockModel` via new `loadModelChainForBlock()` helper.
     - `blocks.Registry.SetDecorationClassifier(fn)` — a callback consulted **only** the first
       time a name *not already in blocks.json* is seen (inside the existing write-lock path
       in `Registry.ID()`). Blocks explicitly listed in `blocks.json` are never
       second-guessed.
     - `app.go` sets this classifier right after the texture store opens:
       `reg.SetDecorationClassifier(func(name string) bool { return
       textures.ClassifyDecoration(texStore, name) })`. If texture sources aren't configured,
       no classifier is set and behaviour is exactly as before (unmapped block = opaque cube).
   - Verified against the *actual currently-explored world* via the `/api/blocks/unknown`
     endpoint — it genuinely contains `biomesoplenty:toadstool`, `spawn:fall_dragon_reeds`,
     `spawn:spring_dragon_reeds`, `biomesoplenty:sprout`/`wildflower`/`violet`/`reed`,
     `supplementaries:wild_flax`, `farmersdelight:wild_carrots`/`wild_onions`/`tomatoes`, etc.
     Re-fetched a 5x5 grid of isometric tiles across a large explored area (forest, water,
     a tower structure) and visually confirmed zero black holes / floating cubes anywhere.

## Current state / how to pick this up

- **Build is clean**: `cd backend && go build ./... && go vet ./... && go test ./...` all
  pass. No test files were left behind (temporary `*_test.go` diagnostic files used during
  this session were always deleted afterward — this project's established convention; do the
  same if you add more).
- **A test server is running** (or was, when this session ended) at `http://localhost:18095/`,
  built from `backend/cmd/minecraft-map` to
  `<scratchpad>/minecraft-map.exe`, using config
  `<scratchpad>/textured-test-config.yml`, which points at:
  - Real world save: `C:/Users/Daniel/curseforge/minecraft/Instances/ff/saves/New World`
  - Real texture sources: the 1.21.1 client jar + the `ff` instance's `mods/` folder
  - `frontendDir` pointing at `frontend/dist` so the UI is served from the same origin
  - Tile cache at `<scratchpad>/data/tiles-textured` — **delete this directory** and restart
    the server after any further rendering-logic change, or you'll see stale cached tiles.
  - `<scratchpad>` = `C:\Users\Daniel\AppData\Local\Temp\claude\C--Users-Daniel-Downloads-mapviewer\1ee572de-dc0a-445b-8094-620072fd09b9\scratchpad`
    (this path is specific to the session that created it — a new session will have a
    different scratchpad path; recreate the config there or somewhere durable if it's gone).
- **Not yet addressed, flagged but not fixed**: a pre-existing frontend bug where setting
  `map.minZoom` to non-zero in config causes `engine.zoom()` to report a wrong value
  (OpenLayers treats the `zoom:` View option as an index into the custom `resolutions` array,
  only correct when `minZoom=0`). Worked around in the test config by setting `minZoom: 0`.
  User has not explicitly confirmed whether/when to fix this for real.
- **Known accepted limitation**: `minecraft:chain` (and only chain, of the blocks checked so
  far) will be misclassified as a ground decoration due to unavoidable geometric ambiguity —
  see the long comment on `isBillboardPlane` in `backend/internal/textures/model.go`.
- **blocks.json**: `config/blocks.json` and `backend/internal/blocks/data/blocks.json` must
  always be kept byte-identical (the latter is `go:embed`-ed as the default; the former is
  what's actually loaded at runtime per `config.example.yml`). Verify with `diff` after any
  edit. Manually-listed vanilla flora entries (short_grass, fern, saplings, crops, flowers,
  etc.) still carry explicit `"transparent": true, "decoration": true` — this is fine/correct,
  it's just redundant now with the automatic classifier for anything not already listed. Not
  worth removing; it's a fast, zero-I/O path for the common vanilla case and documents intent.

## Key files to know

- `backend/internal/textures/model.go` — blockstate/model resolution, the billboard-shape
  classifier, `ClassifyDecoration`.
- `backend/internal/textures/source.go` — jar/zip layered reading.
- `backend/internal/textures/atlas.go` — texture decode + mip chain + tint/overlay compositing
  data types (`FaceTexture`, `BlockFaces`, `Set`).
- `backend/internal/blocks/registry.go` — `Block.Transparent`/`Decoration`, the
  `DecorationClassifier` hook.
- `backend/internal/world/surface.go` — `ChunkSurface`/`Column`/`Surface`, now carrying
  `Decoration` alongside `Block`.
- `backend/internal/mcworld/chunk.go` — the actual top-down world scan; `palEntry`,
  `allTransparent` bulk-skip, the per-column decoration tracking loop.
- `backend/internal/render/shader.go` — `baseColor` vs `topColor` vs `blockColor`,
  `FaceSampler` (the perf-critical per-texel sampling entrypoint used by both renderers).
- `backend/internal/render/topdown.go`, `iso.go` — call `Shader.FaceSampler`, unchanged by the
  decoration work (it was designed to be transparent to them).
- `backend/internal/app/app.go` — where texture sources are opened and the classifier is
  wired up.
- `config/blocks.json` / `backend/internal/blocks/data/blocks.json` — keep in sync.

## If asked to continue this specific thread

Good next questions to ask the user, if they raise something in this area again:
- Do they want `minecraft:chain` specifically special-cased (e.g. a tiny hardcoded denylist
  of `{"minecraft:chain"}` in the classifier) despite the "no hand-curated lists" goal? It's a
  one-line, one-name exception, arguably fine since it's not a *plant* mod-proliferation
  problem — there's only ever going to be one vanilla chain.
- Do they want the `map.minZoom` frontend bug fixed for real (not worked around via config)?
- Do they want this deployed somewhere persistent rather than a scratchpad test server?
