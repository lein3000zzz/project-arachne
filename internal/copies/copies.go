// Package copies tells when two texts are the same text: a mirror, a
// syndicated article, the same docs page for two versions. A small model reads
// a handful of excerpts; two of them saying the same words waste a place, and
// make one source look like two that agree.
//
// Texts are compared as sets of four-word shingles. Whole pages are kept as a
// MinHash sketch, small enough to store beside a page's review; excerpts at
// answer time are compared exactly.
package copies

import (
	"encoding/binary"
	"hash/fnv"
	"strings"
	"unicode"
)

const (
	shingleWords = 4
	// Below this, a text is too short to tell a copy from a common phrase.
	minShingles = 16
	sketchSize  = 64
	// Estimated resemblance from which two pages are one text. Edits between
	// versions, a changed date or a different footer stay above it.
	pageResemblance = 0.7
	// Share of the smaller excerpt set found in the other.
	excerptOverlap = 0.8
)

// Set is a text's distinct shingles.
type Set map[uint64]struct{}

// SetOf shingles each text on its own: a window across two texts - chunks of a
// page - would be a phrase neither holds.
func SetOf(texts ...string) Set {
	set := Set{}

	for _, text := range texts {
		words := wordsOf(text)
		for i := 0; i+shingleWords <= len(words); i++ {
			set[shingle(words[i:i+shingleWords])] = struct{}{}
		}
	}

	return set
}

// Usable says whether the set is large enough to compare.
func (s Set) Usable() bool { return len(s) >= minShingles }

// Repeats says whether one excerpt set is, for the most part, in the other:
// a page and its copy matched on chunks cut at slightly different places, or
// one holding a little more than the other.
func Repeats(a, b Set) bool {
	if !a.Usable() || !b.Usable() {
		return false
	}

	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}

	shared := 0

	for h := range small {
		if _, ok := large[h]; ok {
			shared++
		}
	}

	return float64(shared) >= excerptOverlap*float64(len(small))
}

// Sketch is a page's MinHash: the smallest shingle under each of sketchSize
// hash functions. The share of places two sketches agree on estimates the
// Jaccard resemblance of the pages' shingle sets.
type Sketch []uint32

// SketchOf is nil when the set is too small to say anything.
func SketchOf(set Set) Sketch {
	if !set.Usable() {
		return nil
	}

	sketch := make(Sketch, sketchSize)
	for i := range sketch {
		sketch[i] = ^uint32(0)
	}

	for h := range set {
		for i := range sketch {
			if v := uint32(mix(h ^ seeds[i])); v < sketch[i] {
				sketch[i] = v
			}
		}
	}

	return sketch
}

// Resemblance estimates the Jaccard resemblance of the pages behind two
// sketches; 0 when either is missing.
func Resemblance(a, b Sketch) float64 {
	if len(a) != sketchSize || len(b) != sketchSize {
		return 0
	}

	same := 0

	for i := range a {
		if a[i] == b[i] {
			same++
		}
	}

	return float64(same) / sketchSize
}

// Same says whether two pages are one text.
func Same(a, b Sketch) bool { return Resemblance(a, b) >= pageResemblance }

// Bytes is the sketch as stored; empty for a page too short to sketch, which
// is still a page looked at.
func (s Sketch) Bytes() []byte {
	out := make([]byte, 4*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint32(out[4*i:], v)
	}

	return out
}

// SketchFrom reads a stored sketch; nil for one of another size.
func SketchFrom(b []byte) Sketch {
	if len(b) != 4*sketchSize {
		return nil
	}

	s := make(Sketch, sketchSize)
	for i := range s {
		s[i] = binary.LittleEndian.Uint32(b[4*i:])
	}

	return s
}

func wordsOf(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func shingle(words []string) uint64 {
	h := fnv.New64a()
	for _, w := range words {
		h.Write([]byte(w))
		h.Write([]byte{0})
	}

	return mix(h.Sum64())
}

// mix is splitmix64's finaliser: FNV's low bits are too alike for MinHash.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31

	return x
}

var seeds = func() [sketchSize]uint64 {
	var out [sketchSize]uint64

	x := uint64(0x9e3779b97f4a7c15)
	for i := range out {
		x += 0x9e3779b97f4a7c15
		out[i] = mix(x)
	}

	return out
}()

// Bands of a sketch, for finding pages that may be the same text without
// comparing every pair: pages above the resemblance share a band all but
// surely, and most that share one are close.
const (
	bandRows = 4
	bands    = sketchSize / bandRows
)

// Candidates are the pairs of sketches, by index, that share a band; i < j,
// each pair once. Missing sketches have none.
func Candidates(sketches []Sketch) [][2]int {
	buckets := map[[bandRows + 1]uint32][]int{}

	for i, s := range sketches {
		if len(s) != sketchSize {
			continue
		}

		for b := range bands {
			var key [bandRows + 1]uint32
			key[0] = uint32(b)
			copy(key[1:], s[b*bandRows:(b+1)*bandRows])
			buckets[key] = append(buckets[key], i)
		}
	}

	seen := map[[2]int]bool{}

	var out [][2]int

	for _, members := range buckets {
		for x := range members {
			for y := x + 1; y < len(members); y++ {
				pair := [2]int{members[x], members[y]}
				if !seen[pair] {
					seen[pair] = true
					out = append(out, pair)
				}
			}
		}
	}

	return out
}
