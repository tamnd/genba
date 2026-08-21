// Package thumb makes a small image out of a large one.
//
// It exists because a 48 pixel tile for a two megabyte screenshot used to cost
// two megabytes, and a page of twenty image results cost forty. The bytes a
// browser is asked to move are not visible in any latency measurement of the
// JSON that named them, which is how a slow page stayed slow while every number
// on the dashboard was inside its budget.
//
// This is the first package that decodes a file the corpus handed us, so it is
// written on the assumption that some of those files are hostile. Three rules
// follow from that and none of them is optional.
//
// The header is read before the pixels are. [image.DecodeConfig] reports the
// dimensions without allocating the image, so a file claiming sixty thousand
// pixels on a side is refused for the price of reading its first few bytes
// rather than for the price of thirteen gigabytes.
//
// Concurrency is capped, because the memory ceiling is not per request. A
// decoded image costs four bytes a pixel and the real bound is that number
// times how many decodes are in flight, so one semaphore holds the worst case
// to [MaxPixels] times four times [Concurrency], which is a few hundred
// megabytes rather than however many requests happened to arrive at once.
//
// Every refusal is the same refusal. A hostile image, a text file with a .png
// on the end and a format nobody taught us to read all produce
// [ErrUnrenderable], and the endpoint above turns that into the same 404 a
// document that does not exist produces.
package thumb

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"

	// The other formats the standard library reads. PNG is imported above for
	// the encoder and registers its decoder along the way. Anything not on this
	// list is a refusal, which the interface renders as the kind icon it
	// already has.
	_ "image/gif"
	_ "image/jpeg"
)

// Sizes are the widths a thumbnail may be asked for, in pixels.
//
// Three fixed sizes rather than a number in the query string, so the cache
// holds three entries per document instead of one per distinct request, and so
// a request cannot ask the server to do a piece of work of its own choosing. A
// list tile asks for 48, or 96 where the device pixel ratio is two, and a grid
// cell asks for 256.
var Sizes = []int{48, 96, 256}

// MaxPixels is the largest source image that will be decoded.
//
// Twenty four megapixels is more than any photograph in a corpus and a hundred
// times less than the largest number a PNG header can claim. A decoded image of
// that size is ninety six megabytes, which times [Concurrency] is the worst
// case this package can cost.
const MaxPixels = 24_000_000

// Concurrency is how many images may be decoded at once.
//
// Four rather than one because a thumbnail is generated once and then served
// from a cache, so the queue is short and a single decoder would make the first
// visit to a page of images serial. Four rather than the number of cores
// because the bound being protected is memory rather than time.
const Concurrency = 4

var sem = make(chan struct{}, Concurrency)

// ErrUnrenderable is every refusal.
//
// A file that is not an image, an image in a format we do not read, an image
// with no pixels and an image that claims more pixels than [MaxPixels] all
// produce this one error, because a caller who can tell those apart learns
// something about a document from an endpoint whose job is to say nothing.
var ErrUnrenderable = errors.New("thumb: nothing to render")

// Valid reports whether size is one of [Sizes].
func Valid(size int) bool {
	for _, s := range Sizes {
		if s == size {
			return true
		}
	}
	return false
}

// Thumbnail is a rendered image and the box it occupies.
type Thumbnail struct {
	Bytes         []byte
	Width, Height int
}

// Render scales raw down so that its longest side is size pixels.
//
// The result is a PNG, whatever the source was. A thumbnail at these sizes is a
// few kilobytes either way, PNG is lossless so a diagram with text in it stays
// readable, and one output format means the caller has one content type to
// declare rather than a mapping to keep in step with the decoders above.
//
// It never scales up. An image already smaller than size is re-encoded at its
// own dimensions, because returning a blurry enlargement of a 32 pixel icon
// would be worse than returning the icon.
func Render(ctx context.Context, raw []byte, size int) (Thumbnail, error) {
	if !Valid(size) {
		return Thumbnail{}, ErrUnrenderable
	}

	// The header first, and on its own. Everything about this call is cheap:
	// it reads a few dozen bytes, allocates nothing that scales with the
	// image, and is the only thing standing between a corpus and a decode of
	// whatever size a stranger wrote into a length field.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	switch {
	case err != nil:
		return Thumbnail{}, ErrUnrenderable
	case cfg.Width <= 0 || cfg.Height <= 0:
		return Thumbnail{}, ErrUnrenderable
	case int64(cfg.Width)*int64(cfg.Height) > MaxPixels:
		return Thumbnail{}, ErrUnrenderable
	}

	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return Thumbnail{}, ctx.Err()
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		// A file whose header parsed and whose pixels did not is a truncated
		// upload or a deliberate one, and it is the same answer either way.
		return Thumbnail{}, ErrUnrenderable
	}

	w, h := fit(src.Bounds().Dx(), src.Bounds().Dy(), size)
	if w <= 0 || h <= 0 {
		return Thumbnail{}, ErrUnrenderable
	}

	var out bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(&out, box(src, w, h)); err != nil {
		return Thumbnail{}, ErrUnrenderable
	}
	return Thumbnail{Bytes: out.Bytes(), Width: w, Height: h}, nil
}

// fit is the destination size, preserving the aspect ratio and never enlarging.
func fit(w, h, size int) (width, height int) {
	if w <= size && h <= size {
		return w, h
	}
	if w >= h {
		return size, max(1, h*size/w)
	}
	return max(1, w*size/h), size
}

// box averages the source pixels that land in each destination pixel.
//
// This is the right filter for downscaling and it is the whole of the scaler.
// A general purpose resampler is better at the general case, and the general
// case is enlarging and arbitrary ratios, neither of which happens here: we
// only ever shrink, to one of three sizes. Nearest neighbour would be shorter
// still and it is what makes downscaled text unreadable, because it throws away
// every source pixel it does not happen to land on.
//
// The loop walks the source once rather than the destination, so a twenty four
// megapixel photograph is read a single time whatever size is being asked for.
// Colours are averaged with alpha premultiplied, which is what keeps a
// transparent edge from pulling black into the pixels beside it.
func box(src image.Image, w, h int) *image.NRGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()

	sums := make([]uint64, w*h*4)
	counts := make([]uint32, w*h)
	for y := range sh {
		dy := y * h / sh
		for x := range sw {
			i := dy*w + x*w/sw
			r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			sums[i*4+0] += uint64(r)
			sums[i*4+1] += uint64(g)
			sums[i*4+2] += uint64(bl)
			sums[i*4+3] += uint64(a)
			counts[i]++
		}
	}

	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range counts {
		n := uint64(counts[i])
		if n == 0 {
			// Only reachable when a destination row or column caught no source
			// pixel, which cannot happen while this never enlarges, and which
			// would be a transparent stripe rather than a panic if it ever did.
			continue
		}
		a := sums[i*4+3] / n
		p := dst.Pix[i*4:]
		p[0] = unpremultiply(sums[i*4+0]/n, a)
		p[1] = unpremultiply(sums[i*4+1]/n, a)
		p[2] = unpremultiply(sums[i*4+2]/n, a)
		p[3] = uint8(a >> 8)
	}
	return dst
}

// unpremultiply turns an averaged premultiplied channel back into a plain one.
func unpremultiply(v, a uint64) uint8 {
	if a == 0 {
		return 0
	}
	v = v * 0xffff / a
	if v > 0xffff {
		v = 0xffff
	}
	return uint8(v >> 8)
}
