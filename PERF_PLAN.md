# Performance plan: make the isometric map feel instant

A work plan for whoever picks this up next. Written after adding the Y-slice
feature, which exposed how expensive a cold isometric tile really is.

The target: panning, zooming and dragging the Y slider all feel immediate,
i.e. new tiles appear in well under ~100 ms rather than in seconds.

---

## 1. Where we are, measured

All numbers from this machine against the real world at
`C:/Users/Daniel/curseforge/minecraft/Instances/ff/saves/New World`
(95 region files, ~1200 tiles and ~80 MB currently in `config/data/tiles`),
`render.isoVoxel: true`, `textures.enabled: true` with 176 sources.

| Case | Time |
|---|---|
| Any tile already in the store or memory cache | **~1.3–1.7 ms** |
| Isometric voxel tile, cold, zoom 8 (base zoom, renders from world data) | ~0.7–1.5 s |
| Isometric voxel tile, cold, zoom 7 (composite of 4 children) | ~2.4 s |
| Sliced tile before the footprint fix (zoom 8 / zoom 7) | 16–21 s / 35 s |

Two things follow:

1. **A warm tile is already instant.** The entire problem is cold renders.
   Every optimisation below is either "make a cold render cheaper" or "make
   fewer things cold".
2. **The slice was pathologically slow and is now merely slow.** Clamping the
   voxel footprint to the cut (`attachVolume` in
   `backend/internal/tiles/manager.go`) took it from 16–21 s to ~0.75 s, a
   ~22x win. That same idea generalises, and it is where §4 starts.

### Why an isometric tile is expensive at all

In a 2:1 dimetric projection, **elevation shifts terrain along the view ray**:
one Y level moves a column one block in both X and Z on screen. So the world
rectangle that can project into a single tile grows linearly with the
elevation band you allow for — and its *area*, i.e. the number of chunks to
read and decode, grows roughly with the square of it.

`render.Iso.SurfaceBounds` is currently called with the dimension's full range:

```go
// backend/internal/tiles/manager.go, renderDirect
bounds = iso.SurfaceBounds(req.Pos, dim.MinY, dim.MaxY)   // -64 .. 320
```

That is a **384-level band** for a world whose terrain almost entirely occupies
maybe 60–200. Every isometric tile therefore reads a footprint sized for
hundreds of levels of terrain that cannot possibly appear in it. This is the
single largest cost in the system and §4.1 attacks it.

---

## 2. Ground rules

- **Measure before and after, every time.** §3 exists so this is cheap. Do not
  land an optimisation without a number.
- **Do not regress the default view.** Tiles for the default camera and no
  slice must keep their existing store paths so the ~80 MB already generated
  stays valid. `cache.Key.Variant` is empty for defaults precisely for this.
- **Correctness beats speed.** Two of the bugs found while building the slice
  were silent: a wrong image, not a crash. Verify visually, not just by timing.

---

## 3. Phase 0 — instrumentation (do this first)

Without per-stage numbers the rest is guesswork. A cold tile is roughly:
surface assemble → volume assemble → render → encode. Today only the total is
logged (`"tile generated" … duration_ms`).

1. Time each stage in `Manager.generate` / `renderDirect` / `attachVolume` and
   add them to that existing debug log, plus the chunk count the footprint
   covered — that last number is the one that predicts everything else.
2. Extend `/api/metrics` with per-stage histograms (p50/p95) and counters for
   chunk-cache and voxel-cache hit rates. `Manager` already keeps
   `served`/`generated`/`renderNs`/`failures` atomics; follow that pattern.
3. Add a `minecraft-map bench` subcommand that renders a fixed list of tiles
   (cold, with caches cleared) and prints the table from §1, so any change can
   be compared against the same baseline. `cmd/minecraft-map/main.go` already
   has the subcommand plumbing.

**Acceptance:** `bench` reproduces §1's numbers within noise, and the debug log
shows the four stage timings plus chunk count for one tile.

---

## 4. Phase 1 — stop over-reading the world (biggest win)

### 4.1 Use the dimension's *real* height range, not its theoretical one

Replace `dim.MinY, dim.MaxY` in every isometric footprint calculation with an
observed range: the actual min/max surface height across generated chunks.

- Compute it during the world scan (`scan-world` already walks regions) and/or
  maintain it as chunks are decoded; persist it next to the tile store so it
  survives restarts, and treat it as a hint — widen on the fly if a chunk
  exceeds it, rather than clipping terrain.
- Feed it into `Iso.SurfaceBounds`, `handlePick`'s footprint, and
  `visibleBlockBounds` on the client.
- Keep a small margin (a few blocks) so a newly built tower does not pop.

**Expected:** if the real band is ~140 levels against 384, the footprint area
falls by roughly 7x, and chunk reads with it. This is the one change most
likely to get cold renders under 200 ms on its own.

### 4.2 Stop padding the band by the worst-case slab depth

`attachVolume` widens the bottom of the band by `IsoVoxelMaxDepth` (64):

```go
loEff := lo - m.Cfg.IsoVoxelMaxDepth
```

That is a safety margin against the deepest slab a chunk could have, but real
slabs are governed by `isoVoxelBelowGround: 16` plus canopy height. Record the
actual maximum slab depth observed per dimension and use that instead, keeping
64 only as a ceiling. Every level removed here shrinks the footprint.

### 4.3 Fix a correctness caveat left by the slice footprint clamp

`attachVolume` now clamps the *volume* band to the slice, but `renderDirect`
still computes the *surface* window from the full band. Columns that are in the
surface window but outside the narrower volume window get `ok == false` from
`Volume.TopY` and fall back to `drawColumn` — the heightmap renderer, which
knows nothing about the slice and draws the column **unsliced**. Spot checks
after the clamp still showed correctly sliced tiles, so this may not bite at
current settings, but the two windows disagreeing is a latent silent-wrong-image
bug.

Fix by clamping both consistently: when sliced, pass the slice as the band
ceiling to `SurfaceBounds` too. This is sound — everything drawn under a slice
appears at an elevation ≤ `sliceY`, so a band of `[dim.MinY, sliceY]` covers
every column that can contribute a visible voxel — and it makes the surface
read cheaper as well. Verify by diffing a sliced tile against the same tile
rendered with the fallback path forced off.

---

## 5. Phase 2 — make fewer things cold

### 5.1 Persist sliced tiles, with a bounded number of variants

Sliced tiles are memory-only today, so **every first visit to a level pays the
full render**, forever. With the slider stepping by 4
(`SLICE_STEP` in `frontend/src/main.ts`) there are at most ~96 levels, and in
practice a user visits a handful.

- Put the slice level into `Request.variant()` so it gets its own store
  directory (`iso_y64`, `iso_sw_y64`, …) and allow `Store.Put`.
- Bound growth: keep only the N most recently used slice variants per
  dimension/camera and delete the rest on a background sweep.
- **Pitfall, learned the hard way:** the store key must distinguish every
  variant it can hold. While sliced tiles were unstorable, `childImage` read
  and wrote the store under a key that ignored the slice — the read silently
  returned unsliced imagery for a composite, and the write would have filed a
  cut-away tile under the unsliced key and corrupted the pyramid for everyone.
  If slice joins the key, that hazard goes away; if you add any *other*
  variant, re-check both directions in `Tile`, `generate` and `childImage`.

### 5.2 Pregenerate the explored region

`pregenerate: false` today, so the first view of anywhere is always cold. The
world is sparse — 95 region files — so a bounded background sweep of the base
zooms is affordable. Run it at a priority the scheduler will starve in favour
of `PriorityUser`, and make it resumable.

### 5.3 Prefetch around the viewport and the slider

- When a tile is requested, enqueue its immediate neighbours at low priority so
  panning lands on warm tiles.
- On a slice change, also enqueue the adjacent levels (`±SLICE_STEP`) so the
  next nudge of the slider is warm.

---

## 6. Phase 3 — make the render loop itself cheaper

Only worth doing once §3 shows how much of the total is actually `render`.

- **Hoist per-block-id lookups.** `drawVoxelColumn` calls `reg.Get(id)` and
  `voxelOccludes` calls `Shader.HasTexture(id)` per voxel. Build a small
  per-tile table indexed by block id (resolved lazily) and reuse it.
- **Reuse the chunk pointer per column.** `Volume.BlockAt` does `chunkAt(x, z)`
  on every call, including the three neighbour probes per voxel. A column's
  vertical loop touches one chunk; resolve it once and index into it directly.
- **Reuse image buffers** across tiles instead of allocating an `image.NRGBA`
  per render.
- **Encoding.** Lossless WebP in pure Go is not cheap. Measure its share; if
  it is material, consider lossy WebP for isometric tiles or a faster encoder.
  Note `m.images` already caches decoded images, so composites should not be
  re-decoding children.

---

## 7. Phase 4 — scheduling and parallelism

- `tiles.workers: 8` in `config/config.yml`, but an earlier run logged
  `workers=24`, so this box has cores to spare. Size it from
  `runtime.NumCPU()` and confirm.
- `concurrentChunkFetch` is 16 *per* `Assemble` call. With several tiles
  rendering at once this oversubscribes the disk. Consider one shared global
  semaphore sized to the machine instead of a per-call limit.
- **Abandon superseded work.** When the slider moves, in-flight renders for the
  old level are pure waste. `Manager` deliberately uses its own `jobCtx` so a
  shared render survives one caller disconnecting — correct for ordinary tiles,
  wrong for a slice variant nobody else wants. Add a generation token per
  (dimension, camera, slice) so stale jobs can be dropped at their next
  checkpoint.

---

## 8. Phase 5 — perceived speed on the client

Cheap, and independent of everything above.

- **Never blank the map while re-rendering.** Keep the previous slice's tiles
  visible until the new ones load (a second tile layer swapped on load, or
  OpenLayers' fade), instead of showing empty tiles.
- **Coarse first, sharp second.** Show a magnified lower-zoom sliced tile
  immediately and replace it when the full-resolution one arrives.
- **Apply on release.** The slider currently applies after a 250 ms debounce on
  `input`; using `change` (fires on release) for the apply and `input` only for
  the label removes even that.
- **Show progress.** The slice control should indicate that tiles are still
  rendering, so a 700 ms wait reads as work rather than as breakage — which is
  exactly how the slow version was reported.

---

## 9. Config to revisit once the above lands

- `tiles.isoBaseZoom: 8` — the zoom rendered from world data. Lower means fewer
  but larger renders and more browser magnification; higher multiplies stored
  tiles ~4x per level. Re-tune after §4.
- `tiles.memoryCacheBytes` (192 MB), `minecraft.chunkCacheBytes` (256 MB),
  `minecraft.voxelCacheBytes` (512 MB) — with hit-rate metrics from §3 these
  can be sized on evidence instead of guesswork.
- `render.isoVoxelMaxDepth: 64` — see §4.2.

---

## 10. Invariants worth not breaking

Collected from bugs actually hit while building the camera and slice features.

- **Zero values must mean "as before".** `render.Iso` is built as a struct
  literal in tests; a sentinel `SliceY` meaning "no slice" broke them
  instantly. `Sliced bool` + `SliceY int` is why the zero value still draws
  whole columns. Same reasoning gives `cache.Key.Variant` its empty default.
- **`ok == false` from `Volume.BlockAt` is not "air".** It means "unknown", and
  the occlusion predicate must treat it as not occluding. The slice returns
  *air with ok == true* above the cut, which is what makes the cut face
  visible; returning `false` would have left it a hole.
- **Composite children inherit the whole request** (`childReq := req` with only
  `Pos` changed), so any new field propagates for free — and any new field that
  affects imagery must therefore also be in the cache key.
- **Camera and slice must travel with `/api/pick`.** A tile and the pick
  resolving a click on it have to agree, or clicks resolve to blocks the user
  cannot see.
