package thumb_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"runtime"
	"testing"

	"github.com/tamnd/genba/thumb"
)

func TestRenderShrinksToTheLongestSide(t *testing.T) {
	raw := pngOf(t, 400, 300, color.NRGBA{R: 0xd0, G: 0x20, B: 0x30, A: 0xff})

	got, err := thumb.Render(t.Context(), raw, 48)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Width != 48 || got.Height != 36 {
		t.Errorf("thumbnail is %dx%d, want 48x36", got.Width, got.Height)
	}
	if len(got.Bytes) >= len(raw) {
		t.Errorf("the thumbnail is %d bytes and the original is %d, which is the whole point of the endpoint", len(got.Bytes), len(raw))
	}

	img, format, err := image.Decode(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatalf("decoding what we produced: %v", err)
	}
	if format != "png" {
		t.Errorf("format is %q, want png", format)
	}
	if b := img.Bounds(); b.Dx() != got.Width || b.Dy() != got.Height {
		t.Errorf("the bytes are %v and the dimensions reported are %dx%d", b, got.Width, got.Height)
	}
	// A downscale of one colour is that colour. It is the cheapest way to catch
	// a scaler that has its channels crossed or its alpha unhandled.
	r, g, b, a := img.At(24, 18).RGBA()
	if r>>8 != 0xd0 || g>>8 != 0x20 || b>>8 != 0x30 || a>>8 != 0xff {
		t.Errorf("the middle pixel is %d %d %d %d, want d0 20 30 ff", r>>8, g>>8, b>>8, a>>8)
	}
}

// Averaging with alpha premultiplied is the difference between a soft edge and
// a black fringe, and a fringe is what a naive average of a transparent pixel
// beside a coloured one produces.
func TestRenderDoesNotPullBlackOutOfTransparency(t *testing.T) {
	// Ninety six wide against a forty eight pixel thumbnail, so every
	// destination pixel averages exactly one transparent column and one opaque
	// one and the expected answer is a number rather than a range.
	src := image.NewNRGBA(image.Rect(0, 0, 96, 96))
	for y := range 96 {
		for x := range 96 {
			// White where it is opaque, and a transparent red that a plain
			// average would drag towards nothing.
			c := color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
			if x%2 == 0 {
				c = color.NRGBA{R: 0xff, G: 0x00, B: 0x00, A: 0x00}
			}
			src.SetNRGBA(x, y, c)
		}
	}
	var raw bytes.Buffer
	if err := png.Encode(&raw, src); err != nil {
		t.Fatalf("encoding the fixture: %v", err)
	}

	got, err := thumb.Render(t.Context(), raw.Bytes(), 48)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(got.Bytes))
	if err != nil {
		t.Fatalf("decoding what we produced: %v", err)
	}
	// Read as NRGBA rather than through RGBA, which premultiplies and would
	// report the fringe this test is looking for even when there is none.
	c, ok := color.NRGBAModel.Convert(img.At(24, 24)).(color.NRGBA)
	if !ok {
		t.Fatal("the NRGBA model converted to something that is not an NRGBA")
	}
	if c.A != 0x7f && c.A != 0x80 {
		t.Errorf("alpha is %d, want half of 255", c.A)
	}
	if c.R < 0xf0 || c.G < 0xf0 || c.B < 0xf0 {
		t.Errorf("the colour is %d %d %d, which has black in it from the transparent pixels", c.R, c.G, c.B)
	}
}

func TestRenderNeverEnlarges(t *testing.T) {
	raw := pngOf(t, 32, 24, color.NRGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xff})
	got, err := thumb.Render(t.Context(), raw, 256)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Width != 32 || got.Height != 24 {
		t.Errorf("thumbnail is %dx%d, want the original 32x24", got.Width, got.Height)
	}
}

func TestOnlyThreeSizesExist(t *testing.T) {
	raw := pngOf(t, 400, 300, color.NRGBA{A: 0xff})
	for _, size := range []int{0, 1, 47, 49, 128, 512, -48} {
		if thumb.Valid(size) {
			t.Errorf("Valid(%d) is true, and the point of an enumeration is that it is not open", size)
		}
		if _, err := thumb.Render(t.Context(), raw, size); !errors.Is(err, thumb.ErrUnrenderable) {
			t.Errorf("Render at %d returned %v, want ErrUnrenderable", size, err)
		}
	}
	for _, size := range thumb.Sizes {
		if !thumb.Valid(size) {
			t.Errorf("Valid(%d) is false for a size the package publishes", size)
		}
	}
}

// The table of files somebody else wrote.
//
// Every one of them is the same refusal, and the assertion that matters as much
// as the error is the allocation counter around the loop: a bound that is only
// checked by a comparison in the code is a bound a refactor removes without
// anybody noticing, and the way it shows up in production is a process that
// dies rather than a test that fails.
func TestHostileFilesAreRefusedWithoutDecodingThem(t *testing.T) {
	files := map[string][]byte{
		"a PNG header claiming sixty thousand pixels a side": pngHeader(60000, 60000),
		"a PNG header claiming no pixels at all":             pngHeader(0, 0),
		"a text file with the magic bytes of nothing":        []byte("this is not an image, it is a sentence"),
		"a GIF whose screen is zero by zero":                 []byte("GIF89a\x00\x00\x00\x00\x00\x00\x00"),
		"a JPEG that stops in the middle":                    truncated(t),
		"an empty file":                                      {},
	}

	// The counter is read after a collection so that the number is allocation
	// by this loop rather than whatever the last test left on the heap.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	for name, raw := range files {
		for _, size := range thumb.Sizes {
			if _, err := thumb.Render(t.Context(), raw, size); !errors.Is(err, thumb.ErrUnrenderable) {
				t.Errorf("%s at %d returned %v, want ErrUnrenderable", name, size, err)
			}
		}
	}

	runtime.ReadMemStats(&after)
	// The largest header in the table asks for 3.6 gigapixels, which is 14
	// gigabytes of image. The only decode that gets past the header check is a
	// sixteen pixel JPEG, so anything above a megabyte here means the ceiling
	// stopped being enforced before the decode.
	const ceiling = 1 << 20
	if grew := after.TotalAlloc - before.TotalAlloc; grew > ceiling {
		t.Errorf("refusing six unrenderable files allocated %d bytes, so something is being decoded before it is checked", grew)
	}
}

// A refusal says the same thing about every file, because the caller of this
// package turns it into a 404 and a caller who could tell the cases apart could
// use the difference to learn what a document is.
func TestEveryRefusalIsTheSameError(t *testing.T) {
	for _, raw := range [][]byte{pngHeader(60000, 60000), []byte("plain text"), {}} {
		_, err := thumb.Render(t.Context(), raw, 48)
		if err == nil {
			t.Fatal("an unrenderable file rendered")
		}
		if err.Error() != thumb.ErrUnrenderable.Error() {
			t.Errorf("the error reads %q, which says which case it was", err)
		}
	}
}

func TestRenderReadsJPEGAndGIFAsWell(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 200, 100))
	for y := range 100 {
		for x := range 200 {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xff})
		}
	}
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, src, nil); err != nil {
		t.Fatalf("encoding the fixture: %v", err)
	}
	got, err := thumb.Render(t.Context(), raw.Bytes(), 96)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got.Width != 96 || got.Height != 48 {
		t.Errorf("thumbnail is %dx%d, want 96x48", got.Width, got.Height)
	}
}

// pngOf is a solid rectangle, encoded.
func pngOf(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, c)
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatalf("encoding a %dx%d fixture: %v", w, h, err)
	}
	return out.Bytes()
}

// pngHeader is a signature and an IHDR chunk and nothing else, which is all
// image.DecodeConfig reads and all a file needs to make a claim about its size.
func pngHeader(w, h uint32) []byte {
	var ihdr bytes.Buffer
	ihdr.WriteString("IHDR")
	_ = binary.Write(&ihdr, binary.BigEndian, w)
	_ = binary.Write(&ihdr, binary.BigEndian, h)
	ihdr.Write([]byte{8, 6, 0, 0, 0}) // eight bits a channel, truecolour with alpha

	var out bytes.Buffer
	out.Write([]byte("\x89PNG\r\n\x1a\n"))
	_ = binary.Write(&out, binary.BigEndian, uint32(ihdr.Len()-4))
	out.Write(ihdr.Bytes())
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(ihdr.Bytes()))
	return out.Bytes()
}

// truncated is a JPEG whose header is honest and whose pixels stop early, which
// is what an interrupted upload leaves in a bucket.
func truncated(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatalf("encoding a JPEG to cut in half: %v", err)
	}
	raw := out.Bytes()
	return raw[:len(raw)/2]
}
