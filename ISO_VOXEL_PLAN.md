# Implementation Plan: Real Voxel Isometric Renderer

**Audience:** an AI agent picking up this work cold. Read this whole document before
touching code. Read `HANDOFF.md` too — it carries project conventions (blocks.json
sync, tile-cache invalidation, test-file hygiene) that still apply.

**Repo:** `C:\Users\Daniel\Downloads\mapviewer` (Go backend + TS/OpenLayers frontend,
not a git repo).

---

## 1. The problem being fixed

The user reported that in the isometric view at `http://localhost:18095/`, tree leaves
appear "continued downwards" as vertical smears, and concluded the isometric view is
not a real representation of the world. **That conclusion is correct.** The isometric
renderer is a heightmap extrusion, not a voxel renderer.

### Mechanism, precisely

`world.ChunkSurface` ([backend/internal/world/surface.go:46-68](backend/internal/world/surface.go))
stores exactly **one block ID and one height per X/Z column**:

```go
Height     [ColumnCount]int16  // Y of the topmost rendered block
Block      [ColumnCount]uint16 // registry index of the visible block
Decoration [ColumnCount]uint16 // a thin plant decal on top of Block
```

Everything below the topmost solid block is discarded in
`(*World).surface` ([backend/internal/mcworld/chunk.go:441-534](backend/internal/mcworld/chunk.go))
before the renderer ever runs. For a column whose top block is oak leaves at Y=90, the
renderer knows `("oak_leaves", 90)` and nothing else — not the trunk, not the air pocket,
not the dirt.

`Iso.drawColumn` ([backend/internal/render/iso.go:135-241](backend/internal/render/iso.go))
then computes an **uncapped** skirt depth:

```go
leftDepth  := r.exposedDepth(surf, a, b+1, y)   // y - neighbour.RenderY()
rightDepth := r.exposedDepth(surf, a+1, b, y)
maxDepth   := max(leftDepth, rightDepth)
```

and fills it by tiling the **surface block's own side texture** once per Y level
([iso.go:232-238](backend/internal/render/iso.go)):

```go
for k := 1; k <= depthPx; k++ {
    tv := (k - 1) % ppu
    setPixel(img, px, py, blocks.Scale(sampleSide(sideTu, tv), shade))
}
```

`sampleSide` resolves `faces.Side` of `c.Block` — the leaves
([shader.go:277-284](backend/internal/render/shader.go)).

Result: canopy at Y=90 beside ground at Y=70 → a 20-block wall of tiled leaf texture
dropping to the ground. Identical mechanism produces the hillside "staircases"; those
merely look plausible because a dirt column's side genuinely is dirt.

### What is and isn't real today

- **Real:** the top-surface silhouette — heights, the block visible from above, biome
  tint, light. All from the actual save.
- **Fabricated:** every vertical face. Trunks, overhangs, cave mouths, cantilevered
  builds, the underside of anything — none of it exists in the data the renderer sees.

The user chose to fix this properly: **a real voxel isometric renderer.**

---

## 2. Hard constraint: you cannot load a full block volume per tile

Work this out before designing anything, because it eliminates the obvious approach.

An iso tile's world window comes from `IsoProjection.WorldFootprint`
([backend/internal/mcmath/iso.go:287-327](backend/internal/mcmath/iso.go)), which
inverts the projection across the **entire** dimension height range. At zoom 8 with a
vanilla `-64..320` overworld:

| quantity | value |
|---|---|
| `TileSpanBlocks(8)` = iso units per tile | 128 |
| `uScale, vScale, yScale` | 0.5, 1.0, 1.0 |
| camera-axis span | `128*0.5 + 128*1.0 + 385 + 2` = **579** |
| window (after `.Expand(1)`) | **581 × 581 blocks**, 335 k columns |
| as a `Surface` (10 B/column) | ~3.4 MB — fine, this is today's cost |
| as a **dense volume**, 384 tall × 2 B | **257 MB per tile** |

257 MB per tile, per worker, is not viable. Two consequences drive the whole design:

1. **Bound the vertical extent.** Store only the top *N* blocks of each column, not the
   whole build height.
2. **Tighten the horizontal window.** 385 of those 579 blocks of overscan exist only
   because `WorldFootprint` assumes terrain could be anywhere in `-64..320`. Real
   terrain occupies ~80 blocks of Y. Fixing this is a ~4-5× win and is mandatory here.

---

## 3. Architecture

**Additive, not a rewrite.** Mirror exactly how texture support was added: a new
optional capability that, when absent, leaves every existing code path byte-identical.

- `world.Surface` **stays** and is still produced for every iso tile. It remains the
  source of biome, water depth, per-column relief shading, hit testing, and the
  deep-basement fallback. Do not change its shape.
- A new `world.ChunkVoxels` / `world.Volume` pair carries actual block data for the top
  *N* layers of each column.
- A new `world.VolumeProvider` **optional** interface. `mcworld.World` implements it;
  `demo.World` does not. When the provider does not implement it (or the feature is
  disabled in config), the iso renderer takes the existing heightmap path unchanged.
- `render.Iso` gains a voxel path. `render.TopDown` is untouched.

### 3.1 Per-chunk voxel slab

```go
// backend/internal/world/voxels.go  (new file)

// ChunkVoxels holds the top Depth layers of every column in one chunk, stored
// relative to each column's own top so the slab stays dense and small no matter
// how much relief the chunk has.
type ChunkVoxels struct {
    Pos   mcmath.ChunkPos
    Depth int // number of Y layers stored per column

    // TopY is the Y of the topmost non-air block in each column -- the canopy,
    // not the ground. This is deliberately NOT ChunkSurface.Height, which is the
    // topmost *solid* block; the gap between them is the whole point.
    TopY [ColumnCount]int16

    // Block and Light are Depth*ColumnCount, indexed layer*ColumnCount + col,
    // where layer = TopY[col] - y. Layer 0 is always the column's top block.
    Block []uint16
    Light []uint8
}
```

**Choosing `Depth` per chunk** — this matters and is easy to get wrong. A fixed depth
anchored at `TopY` fails in forests: a 30-block jungle tree with `Depth=24` never
reaches the ground, so the ground would be missing under trees. Instead:

```
span  = max over columns of (TopY[i] - GroundY[i])   // GroundY = ChunkSurface.Height
Depth = clamp(span + BelowGround, MinDepth, MaxDepth)
```

with `BelowGround = 16`, `MinDepth = 8`, `MaxDepth = 64` (all config, see §6). This
guarantees **every** column reaches at least `BelowGround` layers below its own solid
surface, while a flat plains chunk stores ~17 layers and only a forest chunk pays for 48.

Memory: `Depth * 256 * 3` bytes. 17 layers ≈ 13 KB; 48 layers ≈ 37 KB. Compare
`chunkSurfaceCost` = 2688 B ([world/cache.go:50](backend/internal/world/cache.go)).
Budget a **separate** voxel cache, default 512 MB (§6) — do not share the chunk-surface
LRU, whose sizing assumptions (`capacityBytes / chunkSurfaceCost`) would be wrong.

### 3.2 Window over slabs — no blitting

`Surface.Blit` copies chunk data into a flat window array. **Do not do that for voxels**
— it would recreate the 20 MB-per-tile copy. Instead hold pointers:

```go
// Volume is a rectangular window of chunk voxel slabs. It copies nothing: it
// indexes the cached *ChunkVoxels directly, so the whole window costs one slice
// of pointers regardless of how many blocks it spans.
type Volume struct {
    Bounds     mcmath.BlockBounds
    minCX, minCZ, wCX int
    chunks     []*ChunkVoxels // nil entry = ungenerated or not loaded
    MinY, MaxY int
}

func (v *Volume) BlockAt(x, y, z int) (id uint16, light uint8, ok bool)
func (v *Volume) TopY(x, z int) (int, bool)
```

`BlockAt` = two shifts to get the chunk index, one slice index, a `TopY` lookup to get
the layer, one bounds check, two array reads. Keep it inlineable; it is the hot path.
`ok=false` must mean "outside the stored slab or ungenerated" and callers must treat
that as **not occluding** (see §4.3 — getting this backwards produces holes).

Add `world.AssembleVolume(ctx, p VolumeProvider, dim, bounds, minY, maxY, onError)`
modelled on `world.Assemble` ([surface.go:374-441](backend/internal/world/surface.go)) —
same `concurrentChunkFetch` fan-out, same "absent and malformed chunks are skipped, not
fatal" policy.

### 3.3 Producing slabs from Anvil

Add `(*World).voxels(dc *decodedChunk, cs *world.ChunkSurface, dim) *world.ChunkVoxels`
in `backend/internal/mcworld/`. Put it in a **new file** `voxels.go`; do not tangle it
into `chunk.go`'s `surface()`, which is already dense and correct.

- It needs the `ChunkSurface` for `GroundY` (to size `Depth`), so produce the surface
  first and pass it in. `(*World).ChunkVoxels` should call `decodeChunk` once, then
  `surface(...)`, then `voxels(...)`.
- Descend each column from `topY` (same start as `surface()`: `dc.maxSectionY*16+15`,
  clamped below a Nether ceiling) to find the first non-air block → `TopY[i]`.
  "Non-air" here means `id != blocks.AirID`, **not** `!Transparent` — leaves, glass and
  decorations all count as the column top.
- Then fill `Depth` layers downward, recording block ID and
  `sec.lightAt(x, ly, z)` per voxel.
- Reuse `sec.allTransparent` for the descent to `TopY` only. Do **not** bulk-skip while
  filling layers — a section can be mostly air and still contain the leaves you need.
- An empty (void) column gets `TopY = 0` and all-air layers; the renderer must skip it
  via the `Surface`'s `FlagPresent`, exactly as today.

### 3.4 Provider plumbing

```go
// backend/internal/world/voxels.go
type VolumeProvider interface {
    ChunkVoxels(ctx context.Context, dimension string, pos mcmath.ChunkPos) (*ChunkVoxels, error)
}
```

- `mcworld.World` implements it.
- `world.Cached` implements it by type-asserting `c.inner.(VolumeProvider)`; if the
  assertion fails, return a sentinel `ErrVoxelsUnsupported`. Give it its **own** LRU +
  single-flight `Group` + `absent` set, mirroring the existing three
  ([world/cache.go:32-63](backend/internal/world/cache.go)). Wire the new LRU into
  `Invalidate` and `InvalidateAll` — **forgetting this means live world updates render
  stale geometry forever**, and it will not be obvious.
- `demo.World` deliberately does not implement it.

---

## 4. The renderer

New file `backend/internal/render/iso_voxel.go`. Leave `iso.go`'s existing
`drawColumn` path intact as the fallback.

```go
type Iso struct {
    Shader    *Shader
    Proj      mcmath.IsoProjection
    EdgeSkirt int
    Volume    *world.Volume // nil => existing heightmap path
}
```

`Iso.Render` dispatches: `if r.Volume != nil { return r.renderVoxel(...) }`.

### 4.1 Painter's order — get this right, and test it

Camera looks along `(+a, +y, +b)`. Two voxels project to identical pixels iff they have
equal `a-b` and equal `(a+b)/2 - y`. The voxel at `(a+1, y+1, b+1)` has **exactly** the
same hexagon as `(a, y, b)` — same `u`, same `v`. So the view ray in `(a, y, b)` space is
the diagonal `(+1, +1, +1)`, and depth along the view is:

```
s = a + y + b        // larger s = nearer the camera
```

**The exact painter's order is ascending `s`.** Two voxels with equal `s` never overlap
in their interiors (check `(0,0,0)` vs `(1,-1,0)`: hexagons touch only along `u=0.5`,
measure zero). Overlap at all requires `|Δu| < 2` and `|Δv| < 2`, i.e.
`Δ(a-b) ∈ {-1,0,1}` and `|Δ(a+b)/2 - Δy| < 2` — so only near neighbours can conflict.

**Practical iteration:** keep `iso.go`'s existing structure — sweep depth planes
`d = a+b` from far to near, iterate columns within the plane
([iso.go:117-130](backend/internal/render/iso.go)) — and within each column draw voxels
**bottom-up (ascending y)**. Within a fixed `d`, ascending `y` is exactly ascending `s`.
Across `d` planes the ordering is not literally `s`-ascending, but every pair it gets
"wrong" is provably non-overlapping by the `|Δv| < 2` bound above.

> **Do not take that on faith.** Add a permanent test in
> `backend/internal/render/` that rasterises a small synthetic voxel scene twice — once
> in the `d`-sweep order the renderer uses, once by explicitly sorting all voxels by `s`
> — and asserts the two images are pixel-identical. If they differ, the sweep order is
> wrong and you must switch to an explicit `s` sweep. This test is the single most
> valuable thing in this plan.

### 4.2 Per-voxel drawing

For each column `(a,b)` → world `(x,z)`, with `top = vol.TopY(x,z)` and slab bottom
`floor = top - Depth + 1`:

Iterate `y` from `floor` to `top` (ascending). For each non-air voxel, draw up to three
faces:

| face | draw when | shade |
|---|---|---|
| top | block at `(x, y+1, z)` does not occlude | `render.FaceTop` |
| screen-left (`+b`) | block at `Unrotate(a, b+1)`, same `y`, does not occlude | `render.FaceLeft` |
| screen-right (`+a`) | block at `Unrotate(a+1, b)`, same `y`, does not occlude | `render.FaceRight` |

Match `iso.go`'s existing convention: `leftDepth` uses `(a, b+1)` and `rightDepth` uses
`(a+1, b)` ([iso.go:163-164](backend/internal/render/iso.go)), so **screen-left is the
`+b` face and screen-right is the `+a` face**. Keep it identical or the lighting flips.

**Reuse the existing rasterisation maths verbatim.** `drawColumn` already contains
correct, well-documented code for:

- the top-vertex projection and the `left = topPx - w/2` diamond origin (iso.go:147-151);
- the exact diamond tessellation `jLo = ceil((h-e-1)/2)`, `jHi = (h+e-1)/2`
  (iso.go:193-194) — this is what guarantees no moiré gaps; do not re-derive it;
- top-face texel inversion `fa, fb` (iso.go:206-211);
- side-face texel coordinates `sideTu = i` / `i - ppu` (iso.go:223-227).

The only change is that a side face is now **exactly one Y level tall** (`ppu` pixels,
`tv = 0..ppu-1`) at a known `y`, instead of a `depthPx`-tall run of `(k-1) % ppu` tiling.
That single change is what kills the leaf smear.

### 4.3 Occlusion — and the deep basement

`occludes(x, y, z)` must return **false** when `BlockAt` reports `ok=false`. Outside the
slab we do not know, and guessing "occluded" punches holes in the image; guessing
"not occluded" only costs a few extra drawn faces that get overpainted. Fail open.

Below the slab, terrain still has to be drawn or tall cliffs leave voids. Handle it with
a **basement skirt**: after the voxel loop reaches `floor`, fall through to the existing
`drawColumn` skirt logic, using the deepest stored voxel's side texture, running down to
`max(exposedDepth(left), exposedDepth(right))` clamped to that basement. This preserves
the existing "no arbitrary cap, so tall drops never leave a hole" guarantee
([iso.go:158-165](backend/internal/render/iso.go)) while everything within `Depth` of the
surface is genuine geometry.

**Occlusion predicate.** Add to `blocks.Block`:

```go
// Occludes reports whether this block fully hides what is behind it in the
// isometric view. False for air, water, decorations, and any block whose
// resolved texture has transparent texels.
Occludes bool
```

Phase 1: `Occludes = !Transparent && !Water && !Decoration`. This treats leaves as
opaque — good enough to fix the reported bug, and much cheaper. Phase 4 (§5) refines it.

### 4.4 Shading and colour

Add a voxel-aware sampler beside `FaceSampler`
([shader.go:220-268](backend/internal/render/shader.go)):

```go
func (s *Shader) VoxelSampler(
    surf *world.Surface, x, z, y int,
    blockID uint16, light uint8,
    face FaceKind, mipSize int,
) (sample func(tu, tv int) color.NRGBA, ok bool)
```

- Reuse `faceColor`, `tintFor`, `waterColor`, `alphaOver` unchanged.
- Biome comes from the **column** (`surf.At(x,z).Biome`). Biomes barely vary vertically
  and this keeps `Surface` as the single biome source.
- Keep the "constant sampler for untextured blocks" optimisation
  ([shader.go:249-257](backend/internal/render/shader.go)) — it is what makes a
  no-texture-source deployment cost the same as before, and it matters far more now that
  there are ~25× more faces.
- **Relief shading:** apply `reliefFactor` to **top faces only**, and only for the
  column's topmost voxel. Face shading (`FaceTop/Left/Right`) now carries the 3D form;
  applying column relief to side faces would double-count and look muddy.
- **Water:** water voxels are now real entries in the slab. Keep colouring them through
  `waterColor` with the column's `WaterDepth()` from the `Surface` so the existing
  depth-shaded look survives. Verify a coastline and a deep ocean explicitly — this is
  the most likely place to introduce a visual regression.
- **Decorations:** `ChunkSurface.Decoration` still exists and still works. A decoration
  belongs on the **top face of the column's topmost solid voxel** and nowhere else.
  Preserve the existing invariant from `HANDOFF.md` §4: never on a side face.

### 4.5 Window tightening — mandatory

Per §2 this is not optional. In `Manager.renderDirect`
([tiles/manager.go:344-384](backend/internal/tiles/manager.go)):

1. Assemble the `Surface` over the existing conservative `iso.SurfaceBounds(...)`
   window. Cheap (~3.4 MB) and already happening today.
2. `lo, hi, ok := surf.HeightRange()` ([surface.go:281](backend/internal/world/surface.go)).
3. Recompute a tight block window: `r.Proj.WorldFootprint(mcmath.IsoTileBounds(pos), lo - Depth, hi).Expand(1)`.
   Subtracting `Depth` from `lo` covers the basement skirt.
4. `AssembleVolume` over **that** window only.

For terrain in `y ∈ [50, 130]` this shrinks 579² to ~275² — 4.4× fewer columns. With
`Depth ≈ 25`, per-tile voxel visits land around 1.9 M rather than 8.4 M.

Note the `Surface` window stays conservative and is unchanged. Only the volume window
tightens. Do not tighten the `Surface` window: its overscan is what guarantees adjacent
tiles agree on shared pixels (`iso.go`'s "Seams" doc comment), and voxels inherit that
guarantee only if the *column set* is derived the same way from overlapping data.

**Perf budget.** Expect a direct-rendered iso tile to go from ~10-30 ms to ~80-200 ms.
Log it and check `Stats().AvgRenderMs`. If it is worse than ~300 ms, raise the
`IsoBaseZoom` default from 8 to 9 so fewer levels render directly (composites are
unaffected).

---

## 5. Phasing

Ship and verify each phase before starting the next.

**Phase 1 — data path.** `ChunkVoxels`, `Volume`, `VolumeProvider`, `mcworld` producer,
`Cached` plumbing (including invalidation), `AssembleVolume`, config. No renderer change.
*Accept:* unit tests for slab indexing round-trip and `Depth` sizing; a temporary
diagnostic that dumps a known tree column's voxels and shows leaves above air above a log
above dirt. **Delete that diagnostic afterwards** (project convention, `HANDOFF.md` §"Current state").

**Phase 2 — voxel renderer, opaque only.** `iso_voxel.go`, `VoxelSampler`,
`Block.Occludes`, window tightening, painter's-order test.
*Accept:* the painter's-order equivalence test passes; screenshots of the forest area in
the user's report show trunks and ground under canopies, zero leaf smears; a coastline
and a deep ocean look no worse than before; no seams between adjacent tiles.

**Phase 3 — hit testing.** `mcmath.RayMarch` ([iso.go:397-445](backend/internal/mcmath/iso.go))
still marches a height field, so clicking a canopy returns the ground column. Add a
`VoxelSampler`-backed variant so `/api/pick` returns the leaf block the user actually
clicked. `handlePick` ([api/server.go:662-707](backend/internal/api/server.go)) is the
only caller. Ship Phase 2 without this; note it as a known limitation until done.

**Phase 4 — alpha-aware occlusion (optional).** Real leaf canopies have transparent
texels. Add `Mips.HasAlpha` computed in `decodeMips`
([textures/atlas.go:81-105](backend/internal/textures/atlas.go)) and set
`Block.Occludes = false` for blocks whose resolved top/side textures have alpha. This is
automatic and data-driven — consistent with the project's established "no hand-curated
lists" stance (`HANDOFF.md` §5). Cost: cannot cull behind leaves, so forest tiles get
noticeably slower. Measure before committing.

---

## 6. Config

`backend/internal/config/config.go`. Follow the existing `Textures` section's pattern
([config.go:142-153](backend/internal/config/config.go)): disabled-by-default-safe,
never fatal, additive.

```yaml
render:
  isoVoxel: true            # false => existing heightmap iso renderer
  isoVoxelBelowGround: 16   # layers stored below each column's solid surface
  isoVoxelMinDepth: 8
  isoVoxelMaxDepth: 64      # hard cap on per-chunk slab depth
minecraft:
  voxelCacheBytes: 536870912 # separate from chunkCacheBytes
```

Validation: clamp `MinDepth <= MaxDepth`; if `isoVoxel` is true but the provider is not a
`VolumeProvider` (demo world), log once at info and fall back silently. Never fatal.

Wire it in `app.go` right after the tile manager is built, in the same spirit as the
texture block at [app/app.go:158-182](backend/internal/app/app.go).

While you are in `config.go`: `Render.IsoEdgeSkirt` ([config.go:129](backend/internal/config/config.go))
is **dead** — `NewIso` hardcodes `EdgeSkirt: 4` ([iso.go:54](backend/internal/render/iso.go))
and nothing reads the config value. Either wire it through `NewIso` or delete the knob.
Don't leave it.

---

## 7. Verification

```bash
cd backend && go build ./... && go vet ./... && go test ./...
```

Then, and this is not optional:

1. **Delete the tile cache** and restart the server. Stale cached tiles will make correct
   code look broken. Cache dir per the test config's `tiles.directory`
   (`<scratchpad>/data/tiles-textured` in the prior session).
2. Re-render the exact area from the user's screenshot — dense forest on a hillside — and
   confirm: no downward leaf smears; canopies show trunks and ground beneath; hillside
   strata show real block layers.
3. Fetch a 5×5 grid of adjacent iso tiles and check the shared edges for seams. The
   overscan/agreement argument in `iso.go`'s doc comment must still hold.
4. Check water: a coastline, a deep ocean, a waterfall.
5. Check `/api/stats` → `avgRenderMs` against the §4.5 budget.

---

## 8. Things that will bite you

- **`ok=false` from `BlockAt` means "not occluding."** Reversing this produces holes that
  look like a rasterisation bug and will send you into `drawColumn`'s diamond maths for
  hours. It isn't there.
- **Voxel cache invalidation.** `Cached.Invalidate` / `InvalidateAll` must clear the new
  LRU. Symptom if missed: live world edits update top-down tiles but iso tiles keep old
  geometry indefinitely.
- **`TopY` is not `Height`.** `TopY` = topmost non-air (the canopy). `Height` = topmost
  solid (the ground). Conflating them silently reintroduces the original bug.
- **`Depth` must be driven by the max `TopY - GroundY` span**, not a constant, or trees
  taller than `Depth` lose the ground beneath them.
- **Don't blit voxels into a flat window array.** That is the 20 MB-per-tile mistake §3.2
  exists to avoid.
- **Don't touch the diamond tessellation constants.** `jLo`/`jHi` at
  [iso.go:193-194](backend/internal/render/iso.go) are exact and load-bearing.
- **blocks.json stays byte-identical** between `config/blocks.json` and
  `backend/internal/blocks/data/blocks.json`. Verify with `diff` after any edit.
  (`Block.Occludes` is derived, not a JSON key — don't add one unless you need an override.)
- **Test files:** permanent tests are the convention (`iso_raster_test.go`,
  `chunk_test.go`, `coords_test.go`, `iso_test.go`, `revision_test.go` all exist and
  should stay). Only *ad-hoc diagnostic* `*_test.go` files get deleted. The
  painter's-order test in §4.1 is permanent.
- **Pre-existing, unrelated:** `map.minZoom` non-zero makes `engine.zoom()` report wrong
  values in the frontend (OpenLayers treats `zoom:` as an index into the custom
  `resolutions` array). Worked around with `minZoom: 0`. Not yours to fix unless asked.
- **Known accepted limitation:** `minecraft:chain` is geometrically indistinguishable
  from a flower cross and is classified as a decoration. See the long comment on
  `isBillboardPlane` ([textures/model.go:417-439](backend/internal/textures/model.go)).
  Do not "fix" it — both attempted fixes broke real plants.

---

## 9. Files you will touch

**New**
- `backend/internal/world/voxels.go` — `ChunkVoxels`, `Volume`, `VolumeProvider`, `AssembleVolume`
- `backend/internal/mcworld/voxels.go` — Anvil → slab
- `backend/internal/render/iso_voxel.go` — the voxel renderer

**Modified**
- `backend/internal/world/cache.go` — voxel LRU, single-flight, invalidation
- `backend/internal/render/iso.go` — `Volume` field, dispatch; existing path untouched
- `backend/internal/render/shader.go` — `VoxelSampler`
- `backend/internal/blocks/registry.go` — `Block.Occludes`
- `backend/internal/tiles/manager.go` — volume assembly, window tightening
- `backend/internal/config/config.go` — new knobs, `IsoEdgeSkirt` cleanup
- `backend/internal/app/app.go` — wiring
- `backend/internal/mcworld/world.go` — `ChunkVoxels` provider method

**Read first, do not change**
- `backend/internal/mcmath/iso.go` — projection, footprint, ray march
- `backend/internal/render/topdown.go`, `downsample.go`
- `backend/internal/mcworld/chunk.go` — `surface()` stays as-is

**Frontend:** no changes. Tiles are still 512×512 images on the same pyramid.
