/**
 * Isometric projection, mirroring `backend/internal/mcmath/iso.go`.
 *
 * The renderer uses a 2:1 dimetric projection rather than a true isometric one,
 * because true isometry puts irrational scale factors into the pipeline and
 * lands block edges on fractional pixels. With 2:1 everything falls on exact
 * binary fractions at every zoom, which is what keeps blocks crisp.
 *
 * ```
 * u = (a - b) * ISO_HALF_WIDTH
 * v = (a + b) * ISO_HALF_HEIGHT - y * ISO_BLOCK_HEIGHT
 * ```
 *
 * where (a, b) are the world X/Z axes rotated into camera space. Only
 * {@link rotate} and {@link unrotate} know which camera direction is active, so
 * adding the other three view corners later touches nothing else.
 *
 * `v` grows downward, matching screen space. Map space negates it so that
 * OpenLayers, which wants Y up, sees a consistent world in both modes.
 */

/** u distance from a top-face diamond's centre vertex to its side vertex. */
export const ISO_HALF_WIDTH = 1.0;

/** v distance covered by one step along a camera axis. */
export const ISO_HALF_HEIGHT = 0.5;

/** v distance covered by one Y level. Equal to a full diamond height. */
export const ISO_BLOCK_HEIGHT = 1.0;

/** The four diagonal viewing directions, named by the corner the camera sits over. */
export type IsoCamera = 'se' | 'sw' | 'nw' | 'ne';

/** The camera direction the view starts from. */
export const DEFAULT_CAMERA: IsoCamera = 'se';

/**
 * The four corners in the order a rotate control steps through them, so each
 * press turns the world a quarter turn the same way round.
 */
export const CAMERA_ORDER: readonly IsoCamera[] = ['se', 'sw', 'nw', 'ne'];

/** The corner one quarter turn on from `cam`. */
export function nextCamera(cam: IsoCamera): IsoCamera {
  const i = CAMERA_ORDER.indexOf(cam);
  return CAMERA_ORDER[(i + 1) % CAMERA_ORDER.length];
}

/**
 * Where world north points on screen, in degrees clockwise from straight up.
 *
 * Isometric north is never simply "up": under a 2:1 dimetric projection from
 * the default corner it runs up and to the right, and each rotation moves it
 * again. Derived from the projection itself rather than hard-coded per corner,
 * so a compass built on it cannot disagree with the terrain it labels.
 */
export function northBearingDeg(cam: IsoCamera): number {
  const [u0, v0] = project(cam, 0, 0, 0);
  // North is -Z in Minecraft. `v` grows downward, matching screen Y.
  const [u1, v1] = project(cam, 0, 0, -1);
  return (Math.atan2(u1 - u0, v0 - v1) * 180) / Math.PI;
}

/** Maps world X/Z onto the camera's depth axes. A signed axis swap, so exact. */
export function rotate(cam: IsoCamera, x: number, z: number): [number, number] {
  switch (cam) {
    case 'sw':
      return [z, -x];
    case 'nw':
      return [-x, -z];
    case 'ne':
      return [-z, x];
    default:
      return [x, z];
  }
}

/** Inverts {@link rotate}. */
export function unrotate(cam: IsoCamera, a: number, b: number): [number, number] {
  switch (cam) {
    case 'sw':
      return [-b, a];
    case 'nw':
      return [-a, -b];
    case 'ne':
      return [b, -a];
    default:
      return [a, b];
  }
}

/**
 * Projects a world-space point into isometric space.
 *
 * `y` is a point elevation, not a block index. A block's top face sits at
 * `blockY + 1`; see {@link projectBlockTop}.
 */
export function project(
  cam: IsoCamera,
  x: number,
  y: number,
  z: number,
): [number, number] {
  const [a, b] = rotate(cam, x, z);
  const u = (a - b) * ISO_HALF_WIDTH;
  const v = (a + b) * ISO_HALF_HEIGHT - y * ISO_BLOCK_HEIGHT;
  return [u, v];
}

/** Projects the top vertex of a block's top face. */
export function projectBlockTop(
  cam: IsoCamera,
  x: number,
  blockY: number,
  z: number,
): [number, number] {
  return project(cam, x, blockY + 1, z);
}

/**
 * Inverts the projection for a known elevation.
 *
 * A screen position alone is ambiguous in isometric space: it corresponds to a
 * whole ray through the world. Recovering an actual block therefore needs
 * either a known elevation (this function) or a ray march against terrain
 * heights, which the server performs via `/api/pick`.
 */
export function unproject(
  cam: IsoCamera,
  u: number,
  v: number,
  y: number,
): [number, number] {
  const half = (v + y * ISO_BLOCK_HEIGHT) / ISO_HALF_HEIGHT / 2;
  const du = u / ISO_HALF_WIDTH / 2;
  return unrotate(cam, half + du, half - du);
}

/**
 * Resolves an isometric position against a flat reference plane whose block
 * tops sit at `blockY + 1`.
 *
 * This is the immediate, zero-latency estimate used while the cursor moves. It
 * is only correct where terrain is actually at that elevation, so anything that
 * must be exact -- clicks, the coordinate readout, mode switching -- confirms
 * against the server's ray march instead.
 */
export function unprojectFlat(
  cam: IsoCamera,
  u: number,
  v: number,
  blockY: number,
): [number, number] {
  const [x, z] = unproject(cam, u, v, blockY + 1);
  return [Math.floor(x), Math.floor(z)];
}

/**
 * Converts isometric space to the map coordinates OpenLayers uses.
 *
 * Isometric `v` grows downward and OpenLayers wants Y up, so the sign flips --
 * exactly as map space negates Minecraft Z in top-down mode. Because both modes
 * apply the same downward-to-upward flip, one tile grid and one resolution
 * ladder serve both, and fractional zoom behaves identically in each.
 */
export function isoToMap(u: number, v: number): [number, number] {
  return [u, -v];
}

/** Converts map coordinates back to isometric space. */
export function mapToIso(mapX: number, mapY: number): [number, number] {
  return [mapX, -mapY];
}

/**
 * Projects a Minecraft position straight to map coordinates in isometric mode,
 * which is what overlay markers need in order to sit on the terrain.
 */
export function blockToIsoMap(
  cam: IsoCamera,
  x: number,
  blockY: number,
  z: number,
): [number, number] {
  const [u, v] = projectBlockTop(cam, x, blockY, z);
  return isoToMap(u, v);
}

/**
 * Projects the four corners of a block's top face to map coordinates, in ring
 * order. Used to draw chunk and block grids as true projected parallelograms
 * rather than as approximations.
 */
export function blockTopRing(
  cam: IsoCamera,
  x: number,
  blockY: number,
  z: number,
  sizeX = 1,
  sizeZ = 1,
): Array<[number, number]> {
  const y = blockY + 1;
  const corners: Array<[number, number]> = [
    [x, z],
    [x + sizeX, z],
    [x + sizeX, z + sizeZ],
    [x, z + sizeZ],
  ];
  return corners.map(([cx, cz]) => {
    const [u, v] = project(cam, cx, y, cz);
    return isoToMap(u, v);
  });
}

/**
 * Projects a vertical box's top face plus the two side faces a south-east
 * camera actually sees (+X and +Z), in ring order for each. `topY` and the
 * two bottoms are point elevations (already the "block top" convention, i.e.
 * `blockY + 1`), not block indices. The front (+Z) and side (+X) faces take
 * independent bottoms because each one only needs to reach down to whatever
 * is actually exposed on that side -- a neighbouring box at the same height
 * needs no wall between them at all.
 *
 * Used to draw chunk selection as a real 3D volume sitting on the terrain --
 * a flat top-only ring at one shared elevation reads as a plane floating in
 * mid-air the moment the chunk's actual ground height differs from whatever
 * reference plane the caller picked.
 */
export function blockBoxFaces(
  cam: IsoCamera,
  x: number,
  z: number,
  sizeX: number,
  sizeZ: number,
  topY: number,
  frontBottomY: number,
  sideBottomY: number,
): {
  top: Array<[number, number]>;
  front: Array<[number, number]>;
  side: Array<[number, number]>;
} {
  const x1 = x + sizeX;
  const z1 = z + sizeZ;
  const at = (px: number, py: number, pz: number): [number, number] => {
    const [u, v] = project(cam, px, py, pz);
    return isoToMap(u, v);
  };
  return {
    top: [at(x, topY, z), at(x1, topY, z), at(x1, topY, z1), at(x, topY, z1)],
    front: [at(x, topY, z1), at(x1, topY, z1), at(x1, frontBottomY, z1), at(x, frontBottomY, z1)],
    side: [at(x1, topY, z), at(x1, topY, z1), at(x1, sideBottomY, z1), at(x1, sideBottomY, z)],
  };
}
