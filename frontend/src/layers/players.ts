import VectorLayer from 'ol/layer/Vector';
import VectorSource from 'ol/source/Vector';
import Feature from 'ol/Feature';
import Point from 'ol/geom/Point';
import { Fill, Icon, Stroke, Style, Text } from 'ol/style';
import type { FeatureLike } from 'ol/Feature';

import type { Player } from '../api/types';
import type { MapEngine } from '../map/engine';
import { renderPlayerBody, renderPlayerFace, playerBodyAnchor } from '../render/playerModel';

/**
 * Renders players from their own real skin texture (proxied same-origin via
 * GET /api/skin/{uuid}, resolved server-side from Mojang) rather than a
 * generic marker shape or a third-party render service: a face crop in
 * top-down mode, and a real isometric voxel-box body model -- the same
 * projection and face shading the terrain renderer uses -- in isometric
 * mode. See ../render/playerModel.ts for the body model itself.
 */
const FACE_SIZE = 40;

/**
 * The body model's true on-screen height is capped at MAX_BODY_HEIGHT_BLOCKS
 * Minecraft blocks -- a real player is ~1.8 blocks tall, so 2 is a round
 * upper bound, and also exactly what the box rig in playerModel.ts sums to
 * -- scaled by the same pixels-per-block factor the terrain itself uses
 * (MapEngine.pixelsPerBlock), so the character is never drawn larger than
 * an actual 2-block-tall object would appear at the current zoom, and
 * shrinks and grows exactly as the terrain does.
 *
 * In isometric mode a Y-level spans only half a block-width's worth of
 * screen pixels -- the projection is a 2:1 dimetric, so a block's top face
 * is drawn twice as wide as it is tall (see mcmath.IsoHalfWidth /
 * IsoBlockHeight) -- which is why this divides pixelsPerBlock() by 2 rather
 * than using it directly.
 */
const MAX_BODY_HEIGHT_BLOCKS = 2;
const MIN_BODY_SCALE = 0.02;
const MAX_BODY_SCALE = 2;

/** Vertical gap, in screen pixels, between the isometric nametag and the
 * top of the head below it. */
const NAMETAG_GAP_PX = 14;

/** Loads an image from a same-origin URL (no crossOrigin needed). Rejects
 * on a network error or a 404 (an unresolvable uuid), so callers can fall
 * back cleanly instead of showing a broken-image icon. */
function loadImage(src: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.onload = () => resolve(img);
    img.onerror = () => reject(new Error(`failed to load ${src}`));
    img.src = src;
  });
}

/** How long a player's position takes to glide to a newly received one. */
const INTERPOLATION_MS = 900;

/** Players not heard from for this long are dropped. */
const STALE_MS = 30_000;

interface TrackedPlayer {
  player: Player;
  /** Where the marker is drawn now. */
  fromX: number;
  fromZ: number;
  fromY: number;
  fromRot: number;
  /** Where it is heading. */
  toX: number;
  toZ: number;
  toY: number;
  toRot: number;
  startedAt: number;
  lastSeen: number;
}

/**
 * Player marker layer with motion interpolation.
 *
 * ## Why interpolate
 *
 * Positions arrive every couple of seconds. Drawing them raw would make players
 * teleport between points once per update. Instead each update becomes a target
 * and the marker eases toward it over roughly the update interval, so movement
 * reads as continuous motion rather than as a stutter.
 *
 * The layer requests a redraw only while at least one player is actually mid-
 * transition, so an idle map costs nothing.
 */
export class PlayerLayer {
  readonly layer: VectorLayer<VectorSource>;

  private readonly engine: MapEngine;
  private readonly source = new VectorSource();
  private readonly tracked = new Map<string, TrackedPlayer>();
  private readonly features = new Map<string, Feature>();
  /** Resolved (face, body) Icon pair per uuid, built once the real skin
   * texture has loaded -- see skinIconsFor(). Absent while still loading or
   * if the fetch failed, in which case the marker simply has no image
   * until/unless it succeeds. */
  private readonly skinIcons = new Map<string, { face: Icon; body: Icon; bodyHeightPx: number }>();
  private readonly loadingSkins = new Set<string>();
  private readonly failedSkins = new Set<string>();
  /** Where the body model's own origin (the feet) lands within its canvas;
   * identical for every player since the rig geometry never changes,
   * computed once rather than per skin load. */
  private readonly bodyAnchor = playerBodyAnchor();
  private animating = false;
  private visible = true;

  constructor(engine: MapEngine) {
    this.engine = engine;
    this.layer = new VectorLayer({
      source: this.source,
      zIndex: 40,
      style: (f) => this.style(f),
      // Markers must keep up with the map during pans and zooms, not lag a
      // frame behind it.
      updateWhileAnimating: true,
      updateWhileInteracting: true,
    });
  }

  setVisible(v: boolean): void {
    this.visible = v;
    this.layer.setVisible(v);
  }

  isVisible(): boolean {
    return this.visible;
  }

  /** Returns the tracked players, for the sidebar list. */
  list(): Player[] {
    return [...this.tracked.values()].map((t) => t.player);
  }

  /**
   * Applies a position update.
   *
   * A player already being tracked keeps its currently-drawn position as the
   * start of a new transition, so repeated updates chain smoothly instead of
   * restarting from the last received point.
   */
  update(players: Player[]): void {
    const now = performance.now();
    const seen = new Set<string>();
    // Tracks whether anything actually needs animating, so a poll or
    // broadcast tick that reports the same positions as last time does not
    // spin up the animation loop at all.
    let anyChanged = false;

    for (const p of players) {
      seen.add(p.uuid);
      const existing = this.tracked.get(p.uuid);
      if (!existing) {
        this.tracked.set(p.uuid, {
          player: p,
          fromX: p.x, fromZ: p.z, fromY: p.y, fromRot: p.rotation,
          toX: p.x, toZ: p.z, toY: p.y, toRot: p.rotation,
          startedAt: now,
          lastSeen: now,
        });
        anyChanged = true;
        continue;
      }

      existing.player = p;
      existing.lastSeen = now;

      // Positions arrive on every poll or broadcast tick even when a player
      // has not moved. Restarting the transition toward an unchanged target
      // would still work, but it also resets startedAt, which makes tick()
      // report "moving" -- and keeps requesting animation frames -- for
      // another INTERPOLATION_MS after every single one of those ticks.
      const unchanged =
        p.x === existing.toX &&
        p.z === existing.toZ &&
        p.y === existing.toY &&
        p.rotation === existing.toRot;
      if (unchanged) continue;
      anyChanged = true;

      const t = this.progress(existing, now);
      const cur = this.interpolate(existing, t);
      existing.fromX = cur.x;
      existing.fromZ = cur.z;
      existing.fromY = cur.y;
      existing.fromRot = cur.rot;
      existing.toX = p.x;
      existing.toZ = p.z;
      existing.toY = p.y;
      // Take the shortest way round the compass so a player crossing north
      // does not spin the marker the long way.
      existing.toRot = existing.fromRot + shortestAngle(existing.fromRot, p.rotation);
      existing.startedAt = now;
    }

    for (const [uuid, t] of this.tracked) {
      if (!seen.has(uuid) && now - t.lastSeen > STALE_MS) {
        this.tracked.delete(uuid);
        const f = this.features.get(uuid);
        if (f) {
          this.source.removeFeature(f);
          this.features.delete(uuid);
        }
        anyChanged = true;
      }
    }
    if (anyChanged) this.ensureAnimating();
  }

  /** Removes every tracked player, e.g. when switching dimension. */
  clear(): void {
    this.tracked.clear();
    this.features.clear();
    this.source.clear(true);
  }

  /** Hit-tests a viewport pixel against player markers only, for click-to-
   * jump-to -- distinct from the generic block `pickAt`, which resolves
   * terrain, not overlay features. */
  hitTest(pixel: number[]): Player | null {
    let found: Player | null = null;
    this.engine.map.forEachFeatureAtPixel(
      pixel,
      (f) => {
        found = f.get('player') as Player;
        return true;
      },
      { layerFilter: (l) => l === this.layer, hitTolerance: 6 },
    );
    return found;
  }

  private progress(t: TrackedPlayer, now: number): number {
    const raw = (now - t.startedAt) / INTERPOLATION_MS;
    return raw >= 1 ? 1 : raw <= 0 ? 0 : easeOut(raw);
  }

  private interpolate(t: TrackedPlayer, k: number) {
    return {
      x: t.fromX + (t.toX - t.fromX) * k,
      z: t.fromZ + (t.toZ - t.fromZ) * k,
      y: t.fromY + (t.toY - t.fromY) * k,
      rot: t.fromRot + (t.toRot - t.fromRot) * k,
    };
  }

  /**
   * Advances every marker one frame.
   *
   * Returns whether any player is still moving, so the caller can stop
   * requesting animation frames once the map is at rest.
   */
  tick(): boolean {
    if (!this.visible) return false;
    const now = performance.now();
    let moving = false;

    for (const [uuid, t] of this.tracked) {
      const k = this.progress(t, now);
      if (k < 1) moving = true;
      const cur = this.interpolate(t, k);

      let f = this.features.get(uuid);
      const coord = this.engine.blockToView(cur.x, cur.z, Math.round(cur.y));
      if (!f) {
        f = new Feature({ geometry: new Point(coord) });
        f.setId(uuid);
        f.set('player', t.player);
        f.set('rotation', cur.rot);
        this.features.set(uuid, f);
        this.source.addFeature(f);
      } else {
        (f.getGeometry() as Point).setCoordinates(coord);
        f.set('player', t.player, true);
        f.set('rotation', cur.rot, true);
        f.changed();
      }
    }
    return moving;
  }

  /** Drives the animation loop only while something is actually moving. */
  private ensureAnimating(): void {
    if (this.animating) return;
    this.animating = true;
    const step = () => {
      const moving = this.tick();
      this.engine.map.render();
      if (moving) {
        requestAnimationFrame(step);
      } else {
        this.animating = false;
      }
    };
    requestAnimationFrame(step);
  }

  /** Rebuilds geometry after a projection change. */
  reproject(): void {
    this.tick();
  }

  /**
   * Returns the resolved (face, body) icons for a player's real skin,
   * kicking off the fetch-and-render the first time this uuid is seen.
   * undefined while loading, or if it never resolves (fetch failure or an
   * unresolvable uuid) -- the marker simply shows no image until/unless it
   * succeeds.
   */
  private skinIconsFor(uuid: string): { face: Icon; body: Icon; bodyHeightPx: number } | undefined {
    const cached = this.skinIcons.get(uuid);
    if (cached) return cached;
    if (!this.loadingSkins.has(uuid) && !this.failedSkins.has(uuid)) {
      this.loadingSkins.add(uuid);
      void this.loadSkinIcons(uuid);
    }
    return undefined;
  }

  /** Fetches a player's real skin texture (same-origin, proxied server-side
   * from Mojang) and pre-renders both the face crop and the isometric body
   * model from it exactly once, then triggers a re-style so the
   * now-available icons actually appear. */
  private async loadSkinIcons(uuid: string): Promise<void> {
    try {
      const img = await loadImage(`/api/skin/${uuid}`);
      const skinHeight = img.naturalHeight || img.height;
      const faceCanvas = renderPlayerFace(img, skinHeight, FACE_SIZE);
      const bodyCanvas = renderPlayerBody(img, skinHeight);
      this.skinIcons.set(uuid, {
        face: new Icon({ img: faceCanvas, size: [faceCanvas.width, faceCanvas.height], anchor: [0.5, 0.5] }),
        body: new Icon({ img: bodyCanvas, size: [bodyCanvas.width, bodyCanvas.height], anchor: this.bodyAnchor }),
        bodyHeightPx: bodyCanvas.height,
      });
    } catch {
      this.failedSkins.add(uuid);
    } finally {
      this.loadingSkins.delete(uuid);
      this.features.get(uuid)?.changed();
      this.engine.map.render();
    }
  }

  /**
   * The body icon's current display scale, proportional to how large the
   * terrain itself is currently drawn (engine.pixelsPerBlock()) so the
   * character grows and shrinks with the map instead of staying a fixed
   * screen size at every zoom -- see MAX_BODY_HEIGHT_BLOCKS's doc comment.
   */
  private bodyScale(bodyHeightPx: number): number {
    const displayedHeight = this.engine.pixelsPerBlock() * (MAX_BODY_HEIGHT_BLOCKS / 2);
    const scale = displayedHeight / bodyHeightPx;
    return Math.min(MAX_BODY_SCALE, Math.max(MIN_BODY_SCALE, scale));
  }

  private style(f: FeatureLike): Style {
    const p = f.get('player') as Player;
    const zoom = this.engine.zoom();
    const showLabel = zoom >= this.engine.config.overlays.labelsMinZoom - 1;
    const mode = this.engine.getMode();

    let image: Icon | undefined;
    // The face icon and the (much taller, dynamically-scaled) body model
    // need their label at different offsets so it never overlaps the art.
    let labelOffsetY = -(FACE_SIZE / 2 + 10);
    const icons = p ? this.skinIconsFor(p.uuid) : undefined;
    if (icons) {
      if (mode === 'iso') {
        const scale = this.bodyScale(icons.bodyHeightPx);
        icons.body.setScale(scale);
        image = icons.body;
        // The visible top of the model sits bodyAnchor's fraction of the
        // (scaled) canvas height above the anchor point; NAMETAG_GAP_PX adds
        // clear air between the hologram plate and the head above it.
        labelOffsetY = -(this.bodyAnchor[1] * icons.bodyHeightPx * scale + NAMETAG_GAP_PX);
      } else {
        image = icons.face;
      }
    }

    // Isometric mode gets a Minecraft-style floating nametag -- white text
    // on a dark, semi-transparent "hologram" plate -- instead of top-down's
    // plain stroked label, since it sits over a real 3D-ish character model
    // rather than a flat map background.
    const text = showLabel
      ? mode === 'iso'
        ? new Text({
            text: p?.name ?? '',
            offsetY: labelOffsetY,
            font: '600 12px Inter, system-ui, sans-serif',
            fill: new Fill({ color: '#ffffff' }),
            backgroundFill: new Fill({ color: 'rgba(10,12,20,0.55)' }),
            padding: [3, 6, 3, 6],
          })
        : new Text({
            text: p?.name ?? '',
            offsetY: labelOffsetY,
            font: '600 12px Inter, system-ui, sans-serif',
            fill: new Fill({ color: '#ffe9a8' }),
            stroke: new Stroke({ color: 'rgba(6,8,12,0.92)', width: 3 }),
          })
      : undefined;

    return new Style({ image, text });
  }
}

function easeOut(t: number): number {
  return 1 - Math.pow(1 - t, 3);
}

/** Returns the signed shortest rotation from a to b, in degrees. */
function shortestAngle(a: number, b: number): number {
  let d = ((b - a) % 360 + 540) % 360 - 180;
  if (d === -180) d = 180;
  return d;
}
