/**
 * Tiny inline pixel-art icons, rendered as SVG rather than raster images so
 * they stay crisp at any zoom without shipping image assets. Each glyph is
 * authored as an ASCII grid -- '#' (or a palette key, for multi-colour icons)
 * marks a filled cell, '.' is empty -- which keeps the art readable and
 * editable directly in source instead of hidden inside an SVG path.
 */

/** A single-colour icon (inherits `currentColor`, so it follows button text
 * colour automatically) from a '#'/'.' grid. */
export function pixelIcon(art: string[]): string {
  const h = art.length;
  const w = art[0]?.length ?? 0;
  let rects = '';
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      if (art[y][x] !== '.') rects += `<rect x="${x}" y="${y}" width="1" height="1"/>`;
    }
  }
  return `<svg viewBox="0 0 ${w} ${h}" fill="currentColor" shape-rendering="crispEdges" aria-hidden="true">${rects}</svg>`;
}

/** A multi-colour icon: each non-'.' character is looked up in `palette` for
 * its fill, so e.g. a grass block can mix a green top with a brown body. */
export function pixelIconMulti(art: string[], palette: Record<string, string>): string {
  const h = art.length;
  const w = art[0]?.length ?? 0;
  let rects = '';
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const ch = art[y][x];
      if (ch === '.') continue;
      const color = palette[ch];
      if (!color) continue;
      rects += `<rect x="${x}" y="${y}" width="1" height="1" fill="${color}"/>`;
    }
  }
  return `<svg viewBox="0 0 ${w} ${h}" shape-rendering="crispEdges" aria-hidden="true">${rects}</svg>`;
}

export const ICON_PLUS = pixelIcon([
  '.......',
  '...#...',
  '...#...',
  '.#####.',
  '...#...',
  '...#...',
  '.......',
]);

export const ICON_MINUS = pixelIcon([
  '.......',
  '.......',
  '.......',
  '.#####.',
  '.......',
  '.......',
  '.......',
]);

export const ICON_HOME = pixelIcon([
  '...#...',
  '..###..',
  '.#####.',
  '#######',
  '#.###.#',
  '#.###.#',
  '#######',
]);

export const ICON_FULLSCREEN = pixelIcon([
  '##...##',
  '#.....#',
  '.......',
  '.......',
  '.......',
  '#.....#',
  '##...##',
]);

export const ICON_SELECT = pixelIcon([
  '.#.#.#.',
  '.......',
  '#.....#',
  '.......',
  '#.....#',
  '.......',
  '.#.#.#.',
]);

export const ICON_CLOSE = pixelIcon([
  '#.....#',
  '.#...#.',
  '..#.#..',
  '...#...',
  '..#.#..',
  '.#...#.',
  '#.....#',
]);

export const ICON_CARET_DOWN = pixelIcon(['#####', '.###.', '..#..']);

export const ICON_FLAT = pixelIcon([
  '#######',
  '#..#..#',
  '#..#..#',
  '#######',
  '#..#..#',
  '#..#..#',
  '#######',
]);

export const ICON_ISO = pixelIcon([
  '...#...',
  '..#.#..',
  '.#...#.',
  '#.....#',
  '.#...#.',
  '..#.#..',
  '...#...',
]);

export const ICON_LAYERS = pixelIcon([
  '.######.',
  '.######.',
  '........',
  '.######.',
  '.######.',
  '........',
  '.######.',
  '.######.',
]);

export const ICON_SEARCH = pixelIcon(['.####..', '#....#.', '#....#.', '#....#.', '.####..', '....##.', '.....##']);

export const ICON_PLAYER = pixelIcon([
  '..###..',
  '..###..',
  '.......',
  '#######',
  '...#...',
  '..#.#..',
  '.#...#.',
]);

export const ICON_WARP = pixelIcon([
  '...#...',
  '..###..',
  '.#####.',
  '#..#..#',
  '...#...',
  '...#...',
  '...#...',
]);

export const ICON_COORDS = pixelIcon([
  '...#...',
  '...#...',
  '.......',
  '##...##',
  '.......',
  '...#...',
  '...#...',
]);

/** The brand mark: a simplified grass block, instantly readable as
 * "Minecraft" at a glance without imitating Mojang's actual UI or textures. */
export const ICON_GRASS_BLOCK = pixelIconMulti(
  ['gggggggg', 'gGgggggg', 'gggggGgg', 'ddddDddd', 'ddDdddDd', 'dddddDdd', 'dDdddddd', 'ddddDddd'],
  {
    g: '#6fcf6f',
    G: '#4fae4f',
    d: '#9a6a3d',
    D: '#7c5230',
  },
);
