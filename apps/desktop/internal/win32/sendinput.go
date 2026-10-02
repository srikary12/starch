package win32

// The INPUT structures SendInput takes, and the key sequences fed to it.
//
// Untagged, like keys.go, and for a sharper reason: SendInput validates the
// size of the structure it is handed and does nothing at all if it disagrees,
// returning zero with no further explanation. A layout that is wrong by a byte
// is therefore a shell where the hot key appears to work and nothing is ever
// copied or pasted — on a machine this cannot be run on. The layout is fixed by
// the architecture rather than the operating system, so it can be, and is,
// checked here.

import "unsafe"

const (
	inputKeyboard = 1

	keyEventKeyUp = 0x0002

	vkControl = 0x11
	vkC       = 'C'
	vkV       = 'V'
)

// keybdInput is KEYBDINPUT.
type keybdInput struct {
	vk        uint16
	scan      uint16
	flags     uint32
	time      uint32
	extraInfo uintptr
}

// input is INPUT, whose union is as wide as its largest member, MOUSEINPUT.
//
// The trailing padding is that union's spare room. Without it the structure is
// short and SendInput silently refuses every call.
type input struct {
	kind uint32
	_    uint32 // the union is 8-byte aligned
	ki   keybdInput
	_    [8]byte // MOUSEINPUT is wider than KEYBDINPUT
}

// inputSize is what SendInput is told each element measures.
var inputSize = int32(unsafe.Sizeof(input{}))

// chord builds the key events for one modified keystroke: modifier down, key
// down, key up, modifier up.
//
// The order is not cosmetic. Releasing the modifier before the key would leave
// applications seeing a bare key press, and leaving it held would apply it to
// whatever the user types next.
func chord(modifier, key uint16) []input {
	return []input{
		keyEvent(modifier, 0),
		keyEvent(key, 0),
		keyEvent(key, keyEventKeyUp),
		keyEvent(modifier, keyEventKeyUp),
	}
}

func keyEvent(vk uint16, flags uint32) input {
	return input{kind: inputKeyboard, ki: keybdInput{vk: vk, flags: flags}}
}

func copyChord() []input  { return chord(vkControl, vkC) }
func pasteChord() []input { return chord(vkControl, vkV) }
