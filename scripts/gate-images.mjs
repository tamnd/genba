// The pictures the gate needs and the repository does not have.
//
// The corpus the browser gate runs over is this repository, which is prose and
// code and contains no images at all, so the grid and the thumbnail endpoint had
// nothing to audit. These are written into a gitignored directory before the
// server starts and removed after it stops, which keeps a fixture that exists
// for one script out of the tree everybody else reads.
//
// They are written by hand rather than by a library. A PNG is a signature, a
// header chunk, a deflated block of scanlines and an end marker, all of which
// node already has, and the alternative is a dependency in a repository that
// currently has none for the interface.
//
// The pictures are smooth rather than random on purpose. Noise does not
// compress, so a page of it would be large at every size and would say nothing
// about whether the thumbnail endpoint is doing its job. A gradient behaves the
// way a screenshot does: big at full size, small once it is scaled down.

import { deflateSync } from "node:zlib";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const dir = process.argv[2] || "gate-images";
const count = Number(process.argv[3] || 24);
const WIDTH = 1200;
const HEIGHT = 900;

/** png is one whole file: signature, header, pixels, end. */
function png(seed) {
  const header = Buffer.alloc(13);
  header.writeUInt32BE(WIDTH, 0);
  header.writeUInt32BE(HEIGHT, 4);
  // Eight bits a channel, colour type two, which is red green blue with no
  // alpha. Deflate, the only compression a PNG has, no filtering beyond the per
  // scanline byte below, and no interlacing.
  header.set([8, 2, 0, 0, 0], 8);

  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", header),
    chunk("IDAT", deflateSync(scanlines(seed), { level: 6 })),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}

/**
 * scanlines is the picture, one row at a time with its filter byte in front.
 *
 * Every row is filtered as none, because the data underneath is a gradient and
 * deflate handles a gradient perfectly well on its own. A picture that needed
 * the filters would need a filter chooser, which is a compressor rather than a
 * fixture.
 */
function scanlines(seed) {
  const out = Buffer.alloc(HEIGHT * (1 + WIDTH * 3));
  const hue = (seed * 37) % 256;
  const band = 60 + (seed % 6) * 40;
  let at = 0;
  for (let y = 0; y < HEIGHT; y++) {
    out[at++] = 0;
    for (let x = 0; x < WIDTH; x++) {
      const wave = Math.floor(((x + y) % band) * (255 / band));
      out[at++] = (hue + (x >> 3)) & 0xff;
      out[at++] = (255 - (y >> 2) + wave) & 0xff;
      out[at++] = (hue * 2 + wave) & 0xff;
    }
  }
  return out;
}

/** chunk is a length, a type, the data and a checksum over the last two. */
function chunk(type, data) {
  const head = Buffer.alloc(4);
  head.writeUInt32BE(data.length, 0);
  const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const tail = Buffer.alloc(4);
  tail.writeUInt32BE(crc32(body), 0);
  return Buffer.concat([head, body, tail]);
}

const TABLE = new Uint32Array(256);
for (let n = 0; n < 256; n++) {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  TABLE[n] = c >>> 0;
}

function crc32(buf) {
  let c = 0xffffffff;
  for (const byte of buf) c = TABLE[(c ^ byte) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

// Last, so that everything it calls is defined. A const is not hoisted the way
// a function declaration is, and the checksum table above is a const.
mkdirSync(dir, { recursive: true });
for (let i = 0; i < count; i++) {
  writeFileSync(join(dir, `gatepix-${String(i + 1).padStart(2, "0")}.png`), png(i));
}
console.log(`gate-images: wrote ${count} pictures into ${dir}`);
