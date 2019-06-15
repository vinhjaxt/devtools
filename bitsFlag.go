package devtools

// Bits store flags
type Bits uint8

const (
	// F0 .. Fn Flag list
	F0 Bits = 1 << iota
	F1
	F2
)

// Set store a flag into new bits
func (b Bits) Set(flag Bits) Bits { return b | flag }

// Clear remove a flag return new bits
func (b Bits) Clear(flag Bits) Bits { return b &^ flag }

// Toggle a flag return new bits
func (b Bits) Toggle(flag Bits) Bits { return b ^ flag }

// Has check bits have a flag
func (b Bits) Has(flag Bits) bool { return b&flag != 0 }
