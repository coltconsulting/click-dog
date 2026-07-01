package processor

// ilog10 returns floor(log10(n)) for n > 0.
func ilog10(n int64) int {
	e := 0
	for n >= 10 {
		n /= 10
		e++
	}
	return e
}

// ipow10 returns 10^e.
func ipow10(e int) int64 {
	r := int64(1)
	for range e {
		r *= 10
	}
	return r
}
