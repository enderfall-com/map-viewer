/**
 * Draws a player as a real isometric voxel-box model -- the same 2:1
 * dimetric projection and face-shading the terrain renderer uses -- from
 * their actual skin texture, instead of depending on a third-party render
 * service's opaque framing and visual style.
 *
 * # Geometry
 *
 * The rig is the standard Minecraft player model, in blocks (16 skin pixels
 * = 1 block), feet at the origin:
 *
 * ```
 * head   0.5 x 0.5 x 0.5   y: 1.5 .. 2.0
 * torso  0.5 x 0.75 x 0.25 y: 0.75 .. 1.5
 * arms   0.25 x 0.75 x 0.25 (each) y: 0.75 .. 1.5, either side of the torso
 * legs   0.25 x 0.75 x 0.25 (each) y: 0 .. 0.75
 * ```
 *
 * Total height is exactly 2 blocks (0.75 + 0.75 + 0.5), a real player's
 * ~1.8-block height rounded up.
 *
 * # Pose and camera
 *
 * This does not rotate the model to match the player's actual yaw -- it
 * always renders as if facing +Z (matching this app's only camera,
 * CameraSE, which looks from the +X/+Z corner). Per-player yaw is a
 * reasonable future addition; this keeps the first version's scope to the
 * projection and texturing itself.
 *
 * A box's three visible faces from that camera are: top (+Y), the +Z face
 * (the character's own front), and the +X face (their own left side, since
 * facing +Z/south puts east -- +X -- on their left). Screen-left in this
 * app's convention is the +Z neighbour and screen-right is +X (see
 * `render.Iso`'s doc comment in the backend), so front is drawn
 * screen-left-shaded and the left side screen-right-shaded, matching how a
 * real box would look under the same light.
 *
 * # Skin texture format
 *
 * Handles both the modern 64x64 layout (separate left arm/leg regions) and
 * the legacy 64x32 layout (no left-side regions at all; the classic
 * behaviour of reusing the right-side texture for the left limb is
 * approximated by literally reusing the same regions).
 */
import { project, type IsoCamera } from '../coordinates/iso';

/** Pixels per iso-unit baked into the pre-rendered canvas. Chosen high
 * enough to stay crisp after the map's own zoom-dependent Icon scale (see
 * PlayerLayer.bodyScale) shrinks or grows the whole canvas. */
const RENDER_PPU = 48;

/** Matches render.FaceTop/FaceLeft/FaceRight in the backend exactly, so a
 * player's box model is lit the same way the terrain's blocks are. */
const FACE_TOP = 1.0;
const FACE_FRONT = 0.82; // the +Z face, screen-left shaded
const FACE_SIDE = 0.68; // the +X face, screen-right shaded

/** Outline thickness in native (pre-Icon-scale) pixels, and its colour --
 * the same dark UI accent the name label's own text stroke uses, so the
 * two read as one consistent marker style. */
const OUTLINE_PX = 3;
const OUTLINE_COLOR = 'rgba(10,12,16,0.95)';

/** A skin-texture sub-rectangle: [sx, sy, sw, sh] in skin-pixel units. */
type Rect = [number, number, number, number];

interface BoxFaces {
  top: Rect;
  front: Rect; // +Z
  side: Rect; // +X
}

interface Box {
  x0: number;
  x1: number;
  y0: number;
  y1: number;
  z0: number;
  z1: number;
  faces: BoxFaces;
}

/** One body part's skin regions, modern (64x64) layout. avatarX is the
 * region's left edge in the skin texture; the four side faces (right,
 * front, left, back) sit side by side starting there. */
function limbFaces(originX: number, originY: number, w: number, h: number, d: number): BoxFaces {
  return {
    top: [originX + d, originY, w, d],
    front: [originX + d, originY + d, w, h],
    side: [originX + d + w, originY + d, d, h], // "left" region of the limb
  };
}

/** Builds the full rig for a given skin. legacy selects the 64x32 format,
 * which has no dedicated left arm/leg regions -- the right-side ones are
 * reused, approximating the classic client's mirroring. */
function buildRig(legacy: boolean): Box[] {
  const head: BoxFaces = {
    top: [8, 0, 8, 8],
    front: [8, 8, 8, 8],
    side: [16, 8, 8, 8],
  };
  const torso: BoxFaces = limbFaces(16, 16, 8, 12, 4);
  const rightArm: BoxFaces = limbFaces(40, 16, 4, 12, 4);
  const rightLeg: BoxFaces = limbFaces(0, 16, 4, 12, 4);
  const leftArm: BoxFaces = legacy ? rightArm : limbFaces(32, 48, 4, 12, 4);
  const leftLeg: BoxFaces = legacy ? rightLeg : limbFaces(16, 48, 4, 12, 4);

  const b = (x0: number, x1: number, y0: number, y1: number, z0: number, z1: number, faces: BoxFaces): Box => ({
    x0,
    x1,
    y0,
    y1,
    z0,
    z1,
    faces,
  });

  return [
    // Right leg/arm sit at -X (this character's own right, since facing +Z
    // puts west/-X on their right); drawn first, matching painter's order
    // (ascending x -- see the module doc comment).
    b(-0.125 - 0.125, -0.125 + 0.125, 0, 0.75, -0.125, 0.125, rightLeg),
    b(0.125 - 0.125, 0.125 + 0.125, 0, 0.75, -0.125, 0.125, leftLeg),
    b(-0.375 - 0.125, -0.375 + 0.125, 0.75, 1.5, -0.125, 0.125, rightArm),
    b(-0.25, 0.25, 0.75, 1.5, -0.125, 0.125, torso),
    b(0.375 - 0.125, 0.375 + 0.125, 0.75, 1.5, -0.125, 0.125, leftArm),
    b(-0.25, 0.25, 1.5, 2.0, -0.25, 0.25, head),
  ];
}

/** Multiplies an ImageData-less draw with a flat shade by compositing a
 * solid grey over it with 'multiply', matching blocks.Scale's per-channel
 * multiplication in the backend shader exactly (a multiply blend against
 * grey(f) is precisely base*f per channel). */
function shadeFactorToGrey(f: number): string {
  const v = Math.round(Math.max(0, Math.min(1, f)) * 255);
  return `rgb(${v},${v},${v})`;
}

/** Draws one skin region onto a target parallelogram (given as its three
 * corners: top-left, top-right, bottom-left -- the fourth is implied) using
 * the standard canvas affine-transform texture-mapping technique, then
 * darkens it in place to `shade` via a 'multiply' composite. */
function drawFace(
  ctx: CanvasRenderingContext2D,
  skin: CanvasImageSource,
  rect: Rect,
  p0: [number, number],
  p1: [number, number],
  p2: [number, number],
  shade: number,
): void {
  const [sx, sy, sw, sh] = rect;
  if (sw <= 0 || sh <= 0) return;
  ctx.save();
  ctx.setTransform((p1[0] - p0[0]) / sw, (p1[1] - p0[1]) / sw, (p2[0] - p0[0]) / sh, (p2[1] - p0[1]) / sh, p0[0], p0[1]);
  ctx.imageSmoothingEnabled = false;
  ctx.drawImage(skin, sx, sy, sw, sh, 0, 0, sw, sh);
  if (shade < 1) {
    const prevOp = ctx.globalCompositeOperation;
    ctx.globalCompositeOperation = 'multiply';
    ctx.fillStyle = shadeFactorToGrey(shade);
    ctx.fillRect(0, 0, sw, sh);
    ctx.globalCompositeOperation = prevOp;
  }
  ctx.restore();
}

/** The projected (iso-unit) bounding box of the whole rig, with a 1-native-
 * pixel margin -- shared by renderPlayerBody (to size its canvas) and
 * playerBodyAnchor (to compute the origin's fraction), so the two can never
 * drift out of sync with each other. */
function computeBounds(cam: IsoCamera): { minU: number; maxU: number; minV: number; maxV: number } {
  const rig = buildRig(false); // left/right-region choice never affects geometry, only texture
  let minU = Infinity;
  let maxU = -Infinity;
  let minV = Infinity;
  let maxV = -Infinity;
  for (const box of rig) {
    for (const [x, y, z] of [
      [box.x0, box.y1, box.z0],
      [box.x1, box.y1, box.z0],
      [box.x1, box.y1, box.z1],
      [box.x0, box.y1, box.z1],
      [box.x0, box.y0, box.z1],
      [box.x1, box.y0, box.z1],
      [box.x1, box.y0, box.z0],
    ] as const) {
      const [u, v] = project(cam, x, y, z);
      minU = Math.min(minU, u);
      maxU = Math.max(maxU, u);
      minV = Math.min(minV, v);
      maxV = Math.max(maxV, v);
    }
  }
  const marginIso = 1 / RENDER_PPU;
  return { minU: minU - marginIso, maxU: maxU + marginIso, minV: minV - marginIso, maxV: maxV + marginIso };
}

/**
 * Returns a copy of `source` with a solid-colour outline traced around its
 * silhouette, so a player pops against terrain of almost any colour.
 *
 * Classic sprite-outline technique: stamp the (fully opaque-alpha) source
 * repeatedly around a ring of offsets to dilate its silhouette by
 * `thickness`, flatten that dilation to a solid colour with a 'source-in'
 * composite (which keeps only pixels where the destination -- the ring
 * stamps -- already has alpha), then draw the original back on top,
 * centred. The returned canvas is `thickness` pixels larger on every side;
 * callers that also need the origin's position within it (Icon anchors)
 * must account for that padding.
 */
function withOutline(source: HTMLCanvasElement, color: string, thickness: number): HTMLCanvasElement {
  const w = source.width + thickness * 2;
  const h = source.height + thickness * 2;
  const out = document.createElement('canvas');
  out.width = w;
  out.height = h;
  const ctx = out.getContext('2d');
  if (!ctx) return source;

  const steps = 16;
  for (let i = 0; i < steps; i++) {
    const angle = (i / steps) * Math.PI * 2;
    const dx = Math.round(Math.cos(angle) * thickness);
    const dy = Math.round(Math.sin(angle) * thickness);
    ctx.drawImage(source, thickness + dx, thickness + dy);
  }
  ctx.globalCompositeOperation = 'source-in';
  ctx.fillStyle = color;
  ctx.fillRect(0, 0, w, h);
  ctx.globalCompositeOperation = 'source-over';
  ctx.drawImage(source, thickness, thickness);
  return out;
}

/**
 * Renders a player's full-body isometric model from their real skin
 * texture, outlined so it reads clearly against terrain of any colour. The
 * camera is fixed at CameraSE-equivalent framing (see the module doc
 * comment); cam is accepted for forward compatibility if another camera
 * direction is ever added.
 */
export function renderPlayerBody(skin: CanvasImageSource, skinHeight: number, cam: IsoCamera = 'se'): HTMLCanvasElement {
  const legacy = skinHeight <= 32;
  const rig = buildRig(legacy);
  const { minU, maxU, minV, maxV } = computeBounds(cam);

  const width = Math.max(1, Math.ceil((maxU - minU) * RENDER_PPU));
  const height = Math.max(1, Math.ceil((maxV - minV) * RENDER_PPU));

  const canvas = document.createElement('canvas');
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext('2d');
  if (!ctx) return canvas;

  const toPx = ([u, v]: [number, number]): [number, number] => [(u - minU) * RENDER_PPU, (v - minV) * RENDER_PPU];
  const proj = (x: number, y: number, z: number): [number, number] => project(cam, x, y, z);

  for (const box of rig) {
    const { x0, x1, y0, y1, z0, z1 } = box;
    const [topTL, topTR, , topBL] = [proj(x0, y1, z0), proj(x1, y1, z0), proj(x1, y1, z1), proj(x0, y1, z1)].map(toPx);
    drawFace(ctx, skin, box.faces.top, topTL, topTR, topBL, FACE_TOP);

    const [frontTL, frontTR, frontBL] = [proj(x0, y1, z1), proj(x1, y1, z1), proj(x0, y0, z1), proj(x1, y0, z1)].map(toPx);
    drawFace(ctx, skin, box.faces.front, frontTL, frontTR, frontBL, FACE_FRONT);

    const [sideTL, sideTR, sideBL] = [proj(x1, y1, z0), proj(x1, y1, z1), proj(x1, y0, z0), proj(x1, y0, z1)].map(toPx);
    drawFace(ctx, skin, box.faces.side, sideTL, sideTR, sideBL, FACE_SIDE);
  }

  return withOutline(canvas, OUTLINE_COLOR, OUTLINE_PX);
}

/**
 * Renders a player's face (head front, plus the hat/hair overlay layer when
 * the skin has one) at a fixed pixel size, nearest-neighbour scaled to stay
 * crisp -- a flat crop of an 8x8 skin region, not a smoothed photo.
 */
export function renderPlayerFace(skin: CanvasImageSource, skinHeight: number, size: number): HTMLCanvasElement {
  const canvas = document.createElement('canvas');
  canvas.width = size;
  canvas.height = size;
  const ctx = canvas.getContext('2d');
  if (!ctx) return canvas;
  ctx.imageSmoothingEnabled = false;
  ctx.drawImage(skin, 8, 8, 8, 8, 0, 0, size, size);
  if (skinHeight > 32) {
    // The hat/hair overlay layer only exists in the modern 64x64 format.
    ctx.drawImage(skin, 40, 8, 8, 8, 0, 0, size, size);
  }
  return canvas;
}

/** Where the model's own origin (the feet, (0,0,0)) lands within the canvas
 * renderPlayerBody actually returns, as a fraction -- the correct OL Icon
 * anchor. Computed independently of (and before) an actual render, from the
 * same bounds computeBounds derives, adjusted for the extra OUTLINE_PX
 * padding withOutline adds on every side. */
export function playerBodyAnchor(cam: IsoCamera = 'se'): [number, number] {
  const { minU, maxU, minV, maxV } = computeBounds(cam);
  const innerWidth = (maxU - minU) * RENDER_PPU;
  const innerHeight = (maxV - minV) * RENDER_PPU;
  const [originU, originV] = project(cam, 0, 0, 0);
  const originXPx = (originU - minU) * RENDER_PPU + OUTLINE_PX;
  const originYPx = (originV - minV) * RENDER_PPU + OUTLINE_PX;
  const fx = originXPx / (innerWidth + OUTLINE_PX * 2);
  const fy = originYPx / (innerHeight + OUTLINE_PX * 2);
  return [fx, fy];
}
