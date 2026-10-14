package ledger

import (
	"github.com/pinepeakdigital/looper/internal/py"
	"github.com/pinepeakdigital/looper/internal/record"
)

// The Python text model, as far as these two ledgers reach it. Parity is byte for byte, so
// "whitespace", "a line" and "a digit" each mean what they mean to Python 3's str and re,
// not what they mean to Go's strings and regexp — the two disagree on all three.

// isSpace is str.isspace(), which is also what re's \s matches in a str pattern. It is
// record's definition, shared with internal/stats, so the module has one meaning of Python
// whitespace rather than one per port.
var isSpace = record.PySpace

// spaceClass is isSpace as a regexp class body; TestSpaceClassIsIsSpace holds the two equal
// over every code point, since a regexp class cannot call a function.
const spaceClass = `\t\n\x0b\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// digitClass is re's \d in a str pattern: Unicode category Nd, as of the Unicode 16.0 tables
// the oracle's Python carried. Go's \p{Nd} is Unicode 15.0 in go1.24 and lacks the scripts 16.0
// added, so the table is spelled out rather than borrowed.
const digitClass = `\x{30}-\x{39}\x{660}-\x{669}\x{6f0}-\x{6f9}\x{7c0}-\x{7c9}\x{966}-\x{96f}\x{9e6}-\x{9ef}` +
	`\x{a66}-\x{a6f}\x{ae6}-\x{aef}\x{b66}-\x{b6f}\x{be6}-\x{bef}\x{c66}-\x{c6f}\x{ce6}-\x{cef}` +
	`\x{d66}-\x{d6f}\x{de6}-\x{def}\x{e50}-\x{e59}\x{ed0}-\x{ed9}\x{f20}-\x{f29}\x{1040}-\x{1049}` +
	`\x{1090}-\x{1099}\x{17e0}-\x{17e9}\x{1810}-\x{1819}\x{1946}-\x{194f}\x{19d0}-\x{19d9}` +
	`\x{1a80}-\x{1a89}\x{1a90}-\x{1a99}\x{1b50}-\x{1b59}\x{1bb0}-\x{1bb9}\x{1c40}-\x{1c49}` +
	`\x{1c50}-\x{1c59}\x{a620}-\x{a629}\x{a8d0}-\x{a8d9}\x{a900}-\x{a909}\x{a9d0}-\x{a9d9}` +
	`\x{a9f0}-\x{a9f9}\x{aa50}-\x{aa59}\x{abf0}-\x{abf9}\x{ff10}-\x{ff19}\x{104a0}-\x{104a9}` +
	`\x{10d30}-\x{10d39}\x{10d40}-\x{10d49}\x{11066}-\x{1106f}\x{110f0}-\x{110f9}\x{11136}-\x{1113f}` +
	`\x{111d0}-\x{111d9}\x{112f0}-\x{112f9}\x{11450}-\x{11459}\x{114d0}-\x{114d9}\x{11650}-\x{11659}` +
	`\x{116c0}-\x{116c9}\x{116d0}-\x{116e3}\x{11730}-\x{11739}\x{118e0}-\x{118e9}\x{11950}-\x{11959}` +
	`\x{11bf0}-\x{11bf9}\x{11c50}-\x{11c59}\x{11d50}-\x{11d59}\x{11da0}-\x{11da9}\x{11f50}-\x{11f59}` +
	`\x{16130}-\x{16139}\x{16a60}-\x{16a69}\x{16ac0}-\x{16ac9}\x{16b50}-\x{16b59}\x{16d70}-\x{16d79}` +
	`\x{1ccf0}-\x{1ccf9}\x{1d7ce}-\x{1d7ff}\x{1e140}-\x{1e149}\x{1e2f0}-\x{1e2f9}\x{1e4f0}-\x{1e4f9}` +
	`\x{1e5f1}-\x{1e5fa}\x{1e950}-\x{1e959}\x{1fbf0}-\x{1fbf9}`

// The rest of the text model is internal/py's, shared with looper plan; these are its names
// here so the ledger code reads as it did.
type (
	value = py.Value
	obj   = py.Obj
	field = py.Field
)

var (
	decode     = py.Decode
	splitlines = py.Splitlines
	strip      = py.Strip
	encode     = py.Encode
)
