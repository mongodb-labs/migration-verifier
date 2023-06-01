// Package permute implements a generic method for in-place generation
// of all permutations for ordered collections.
package permute

// The algorithm used here is a non-recursive version of Heap's permutation method.
// See http://en.wikipedia.org/wiki/Heap's_algorithm
//
// Here's pseudocode from https://www.cs.princeton.edu/~rs/talks/perms.pdf
//
// generate(int N) {
//   int n;
//   for (n = 1; n <= N; n++) {
//     p[n] = n;
//     c[n] = 1;
//   }
//   doit();
//   for (n = 1; n <= N; ) {
//     if (c[n] < n) {
//       exch(N % 2 ? 1 : c, N)
//       c[n]++;
//       n = 1;
//       doit();
//     } else {
//       c[n++] = 1;
//     }
//   }
// }

// Interface is satisfied by types (usually ordered collections)
// which can have all permutations generated by a Permuter.
type Interface interface {
	// Len is the number of elements in the collection.
	Len() int
	// Swap swaps the elements with indexes i and j.
	Swap(i, j int)
}

// New gives a Permuter to generate all permutations
// of the elements of v.
func New(v Interface) *Permuter {
	return &Permuter{iface: v}
}

// A Permuter holds state about an ongoing iteration of permutations.
type Permuter struct {
	iface    Interface
	started  bool
	finished bool
	n        int
	i        int
	p        []int
	c        []int
}

// Permute generates the next permutation of the contained collection in-place.
// If it returns false, the iteration is finished.
func (p *Permuter) Permute() bool {
	if p.finished {
		panic("Permute called on finished Permuter")
	}
	if !p.started {
		p.started = true
		p.n = p.iface.Len()
		p.p = make([]int, p.n)
		p.c = make([]int, p.n)
		for i := range p.p {
			p.p[i] = i
			p.c[i] = 0
		}
		return true
	}
	for {
		if p.i >= p.n {
			p.finished = true
			return false
		}
		if c := p.c[p.i]; c < p.i {
			if p.i&1 == 0 {
				c = 0
			}
			p.iface.Swap(c, p.i)
			p.c[p.i]++
			p.i = 0
			return true
		}
		p.c[p.i] = 0
		p.i++
	}
}

type slice[T any] []T

func (s slice[T]) Len() int      { return len(s) }
func (s slice[T]) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// Slice constructs a Permuter for a slice.
func Slice[S ~[]T, T any](s S) *Permuter { return New(slice[T](s)) }