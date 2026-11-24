package profile

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// ---- control messages (client → device), big-endian ----

// Position is a point plus the frame size it refers to.
type Position struct {
	X, Y          int32
	Width, Height uint16
}

func putPos(b []byte, p Position) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(p.X))
	b = binary.BigEndian.AppendUint32(b, uint32(p.Y))
	b = binary.BigEndian.AppendUint16(b, p.Width)
	return binary.BigEndian.AppendUint16(b, p.Height)
}

// U16Fixed encodes a float in [0, 1] (pressure).
func U16Fixed(f float32) uint16 {
	if f >= 1 {
		return 0xffff
	}
	if f <= 0 {
		return 0
	}
	return uint16(f * 65536)
}

// I16Fixed encodes a float in [-1, 1] like the client's sc_float_to_i16fp
// (truncation toward zero, clamped).
func I16Fixed(f float32) int16 {
	i := int32(math.Trunc(float64(f) * 32768))
	if i >= 0x7fff {
		return 0x7fff
	}
	if i < -0x8000 {
		return -0x8000
	}
	return int16(i)
}

func putStr(b []byte, s string, lenBytes int, max int) []byte {
	if max > 0 && len(s) > max {
		s = s[:max]
	}
	switch lenBytes {
	case 1:
		b = append(b, byte(len(s)))
	case 2:
		b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
	default:
		b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	}
	return append(b, s...)
}

func EncodeInjectKeycode(action uint8, keycode, repeat, metastate uint32) []byte {
	b := []byte{InjectKeycode, action}
	b = binary.BigEndian.AppendUint32(b, keycode)
	b = binary.BigEndian.AppendUint32(b, repeat)
	return binary.BigEndian.AppendUint32(b, metastate)
}

func EncodeInjectText(text string) []byte {
	return putStr([]byte{InjectText}, text, 4, InjectTextMaxLength)
}

func EncodeInjectTouch(action uint8, pointerID uint64, p Position, pressure float32, actionButton, buttons uint32) []byte {
	b := []byte{InjectTouchEvent, action}
	b = binary.BigEndian.AppendUint64(b, pointerID)
	b = putPos(b, p)
	b = binary.BigEndian.AppendUint16(b, U16Fixed(pressure))
	b = binary.BigEndian.AppendUint32(b, actionButton)
	return binary.BigEndian.AppendUint32(b, buttons)
}

// EncodeInjectScroll takes scroll amounts in [-16, 16].
func EncodeInjectScroll(p Position, h, v float32, buttons uint32) []byte {
	b := putPos([]byte{InjectScrollEvent}, p)
	b = binary.BigEndian.AppendUint16(b, uint16(I16Fixed(h/16)))
	b = binary.BigEndian.AppendUint16(b, uint16(I16Fixed(v/16)))
	return binary.BigEndian.AppendUint32(b, buttons)
}

func EncodeBackOrScreenOn(action uint8) []byte { return []byte{BackOrScreenOn, action} }
func EncodeEmpty(typ byte) []byte              { return []byte{typ} }
func EncodeGetClipboard(copyKey uint8) []byte  { return []byte{GetClipboard, copyKey} }

func EncodeSetClipboard(seq uint64, paste bool, text string) []byte {
	b := binary.BigEndian.AppendUint64([]byte{SetClipboard}, seq)
	if paste {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	return putStr(b, text, 4, ClipboardTextMaxLength)
}

func EncodeSetDisplayPower(on bool) []byte {
	if on {
		return []byte{SetDisplayPower, 1}
	}
	return []byte{SetDisplayPower, 0}
}

func EncodeUhidCreate(id, vendor, product uint16, name string, desc []byte) []byte {
	b := binary.BigEndian.AppendUint16([]byte{UhidCreate}, id)
	b = binary.BigEndian.AppendUint16(b, vendor)
	b = binary.BigEndian.AppendUint16(b, product)
	b = putStr(b, name, 1, 255)
	b = binary.BigEndian.AppendUint16(b, uint16(len(desc)))
	return append(b, desc...)
}

func EncodeUhidInput(id uint16, data []byte) []byte {
	b := binary.BigEndian.AppendUint16([]byte{UhidInput}, id)
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
	return append(b, data...)
}

func EncodeUhidDestroy(id uint16) []byte {
	return binary.BigEndian.AppendUint16([]byte{UhidDestroy}, id)
}

func EncodeStartApp(name string) []byte { return putStr([]byte{StartApp}, name, 1, 255) }

func EncodeCameraSetTorch(on bool) []byte {
	if on {
		return []byte{CameraSetTorch, 1}
	}
	return []byte{CameraSetTorch, 0}
}

func EncodeResizeDisplay(w, h uint16) []byte {
	b := binary.BigEndian.AppendUint16([]byte{ResizeDisplay}, w)
	return binary.BigEndian.AppendUint16(b, h)
}

func EncodeScanFile(path string) []byte { return putStr([]byte{ScanFile}, path, 4, 256) }

// ErrIncomplete means more bytes are needed for a whole message.
var ErrIncomplete = errors.New("incomplete control message")

func lenField(b []byte, off, n int) (int, error) {
	if len(b) < off+n {
		return 0, ErrIncomplete
	}
	v := 0
	for i := 0; i < n; i++ {
		v = v<<8 | int(b[off+i])
	}
	return v, nil
}

// MessageLen returns the size of the control message at the start of b.
func (s *Spec) MessageLen(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, ErrIncomplete
	}
	t := b[0]
	if _, ok := s.ControlTypes[t]; !ok {
		return 0, fmt.Errorf("control message type %d is not supported by server %s", t, s.Version)
	}
	switch t {
	case InjectKeycode:
		return 14, nil
	case InjectText, ScanFile:
		n, err := lenField(b, 1, 4)
		if err != nil {
			return 0, err
		}
		limit := InjectTextMaxLength
		if t == ScanFile {
			limit = 256
		}
		if n > limit {
			return 0, fmt.Errorf("control message %d: string too long (%d)", t, n)
		}
		return 5 + n, nil
	case InjectTouchEvent:
		return 32, nil
	case InjectScrollEvent:
		return 21, nil
	case BackOrScreenOn, GetClipboard, SetDisplayPower, CameraSetTorch:
		return 2, nil
	case ExpandNotificationPanel, ExpandSettingsPanel, CollapsePanels, RotateDevice,
		OpenHardKeyboardSettings, ResetVideo, CameraZoomIn, CameraZoomOut:
		return 1, nil
	case SetClipboard:
		n, err := lenField(b, 10, 4)
		if err != nil {
			return 0, err
		}
		if n > ClipboardTextMaxLength {
			return 0, fmt.Errorf("clipboard text too long (%d)", n)
		}
		return 14 + n, nil
	case UhidCreate:
		nl, err := lenField(b, 7, 1)
		if err != nil {
			return 0, err
		}
		dl, err := lenField(b, 8+nl, 2)
		if err != nil {
			return 0, err
		}
		return 10 + nl + dl, nil
	case UhidInput:
		n, err := lenField(b, 3, 2)
		if err != nil {
			return 0, err
		}
		return 5 + n, nil
	case UhidDestroy:
		return 3, nil
	case StartApp:
		n, err := lenField(b, 1, 1)
		if err != nil {
			return 0, err
		}
		return 2 + n, nil
	case ResizeDisplay:
		return 5, nil
	}
	return 0, fmt.Errorf("control message type %d: unknown layout", t)
}

// Validate checks that b holds exactly one well-formed control message the
// server version accepts (an unknown type would make the server's
// controller throw and end the session).
func (s *Spec) Validate(b []byte) error {
	n, err := s.MessageLen(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return fmt.Errorf("control message type %d: length %d, expected %d", b[0], len(b), n)
	}
	return nil
}

// ---- device messages (device → client) ----

// DeviceMessage is one decoded device message.
type DeviceMessage struct {
	Type     byte
	Text     string // clipboard
	Sequence uint64 // ack_clipboard
	ID       uint16 // uhid_output
	Data     []byte // uhid_output
	Raw      []byte // the exact wire bytes
}

// DeviceReader splits the control socket's device→client stream into
// whole messages.
type DeviceReader struct {
	r    *bufio.Reader
	spec *Spec
}

// NewDeviceReader wraps r.
func NewDeviceReader(r io.Reader, s *Spec) *DeviceReader {
	return &DeviceReader{r: bufio.NewReaderSize(r, 64*1024), spec: s}
}

func (d *DeviceReader) full(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(d.r, b)
	return b, err
}

// Next reads one message.
func (d *DeviceReader) Next() (*DeviceMessage, error) {
	t, err := d.r.ReadByte()
	if err != nil {
		return nil, err
	}
	if _, ok := d.spec.DeviceMessages[t]; !ok {
		return nil, fmt.Errorf("unknown device message type %d", t)
	}
	m := &DeviceMessage{Type: t}
	raw := []byte{t}
	switch t {
	case DevClipboard:
		h, err := d.full(4)
		if err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(h)
		if n > DeviceClipboardMaxLength {
			return nil, fmt.Errorf("device clipboard too long (%d)", n)
		}
		body, err := d.full(int(n))
		if err != nil {
			return nil, err
		}
		m.Text = string(body)
		raw = append(append(raw, h...), body...)
	case DevAckClipboard:
		h, err := d.full(8)
		if err != nil {
			return nil, err
		}
		m.Sequence = binary.BigEndian.Uint64(h)
		raw = append(raw, h...)
	case DevUhidOutput:
		h, err := d.full(4)
		if err != nil {
			return nil, err
		}
		m.ID = binary.BigEndian.Uint16(h)
		n := binary.BigEndian.Uint16(h[2:])
		body, err := d.full(int(n))
		if err != nil {
			return nil, err
		}
		m.Data = body
		raw = append(append(raw, h...), body...)
	}
	m.Raw = raw
	return m, nil
}

// EncodeDeviceClipboard builds a device clipboard message (tests, fakes).
func EncodeDeviceClipboard(text string) []byte {
	b := binary.BigEndian.AppendUint32([]byte{DevClipboard}, uint32(len(text)))
	return append(b, text...)
}

// EncodeDeviceAckClipboard builds an ack message.
func EncodeDeviceAckClipboard(seq uint64) []byte {
	return binary.BigEndian.AppendUint64([]byte{DevAckClipboard}, seq)
}

// EncodeDeviceUhidOutput builds a uhid output message.
func EncodeDeviceUhidOutput(id uint16, data []byte) []byte {
	b := binary.BigEndian.AppendUint16([]byte{DevUhidOutput}, id)
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
	return append(b, data...)
}

// ---- stream framing (device → client on video/audio sockets) ----

// Packet flags in the media header's first u64.
const (
	FlagSession  = uint64(1) << 63
	FlagConfig   = uint64(1) << 62
	FlagKeyFrame = uint64(1) << 61
	PTSMask      = FlagKeyFrame - 1
)

// Codec ids.
var CodecIDs = map[uint32]string{
	0x68323634: "h264", 0x68323635: "h265", 0x00617631: "av1",
	0x00767038: "vp8", 0x00767039: "vp9",
	0x6f707573: "opus", 0x00616163: "aac", 0x666c6163: "flac", 0x00726177: "raw",
}

// CodecID returns the id for a codec name.
func CodecID(name string) uint32 {
	for id, n := range CodecIDs {
		if n == name {
			return id
		}
	}
	return 0
}

// SessionPacket is the 12-byte video session header.
type SessionPacket struct {
	Width, Height uint32
	ClientResized bool
}

// EncodeSession builds a session packet.
func (s *Spec) EncodeSession(p SessionPacket) []byte {
	flags := uint32(FlagSession >> 32)
	if p.ClientResized && s.Handshake.ResizedFlag {
		flags |= 1
	}
	b := binary.BigEndian.AppendUint32(nil, flags)
	b = binary.BigEndian.AppendUint32(b, p.Width)
	return binary.BigEndian.AppendUint32(b, p.Height)
}

// EncodeMediaHeader builds the 12-byte media packet header.
func EncodeMediaHeader(pts uint64, config, key bool, size uint32) []byte {
	v := pts & PTSMask
	if config {
		v = FlagConfig
	} else if key {
		v |= FlagKeyFrame
	}
	b := binary.BigEndian.AppendUint64(nil, v)
	return binary.BigEndian.AppendUint32(b, size)
}
