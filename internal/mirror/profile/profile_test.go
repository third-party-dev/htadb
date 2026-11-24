package profile

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

type golden struct {
	Control []struct {
		Name, Fn, Hex, Since string
		Args                 map[string]any
	}
	Device []struct {
		Name, Hex, Text, Sequence string
		Type                      byte
		ID                        uint16
		Data                      []byte
	}
	Session []struct {
		Name, Version, Hex string
		Width, Height      uint32
		ClientResized      bool
	}
	Media []struct {
		Name        string
		Pts         uint64
		Config, Key bool
		Size        uint32
		Hex         string
	}
}

func loadGolden(t *testing.T) golden {
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func num(a map[string]any, k string) uint64 {
	switch v := a[k].(type) {
	case float64:
		return uint64(int64(v))
	case string:
		n, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 64)
		return n
	}
	return 0
}

func bytesArg(a map[string]any, k string) []byte {
	var out []byte
	for _, x := range a[k].([]any) {
		out = append(out, byte(x.(float64)))
	}
	return out
}

func encode(fn string, a map[string]any) []byte {
	str := func(k string) string { s, _ := a[k].(string); return s }
	b := func(k string) bool { v, _ := a[k].(bool); return v }
	f := func(k string) float32 { v, _ := a[k].(float64); return float32(v) }
	pos := Position{X: int32(num(a, "x")), Y: int32(num(a, "y")), Width: uint16(num(a, "width")), Height: uint16(num(a, "height"))}
	switch fn {
	case "injectKeycode":
		return EncodeInjectKeycode(uint8(num(a, "action")), uint32(num(a, "keycode")), uint32(num(a, "repeat")), uint32(num(a, "metastate")))
	case "injectText":
		return EncodeInjectText(str("text"))
	case "injectTouch":
		return EncodeInjectTouch(uint8(num(a, "action")), num(a, "pointerId"), pos, f("pressure"), uint32(num(a, "actionButton")), uint32(num(a, "buttons")))
	case "injectScroll":
		return EncodeInjectScroll(pos, f("hscroll"), f("vscroll"), uint32(num(a, "buttons")))
	case "backOrScreenOn":
		return EncodeBackOrScreenOn(uint8(num(a, "action")))
	case "expandNotificationPanel":
		return EncodeEmpty(ExpandNotificationPanel)
	case "expandSettingsPanel":
		return EncodeEmpty(ExpandSettingsPanel)
	case "collapsePanels":
		return EncodeEmpty(CollapsePanels)
	case "getClipboard":
		return EncodeGetClipboard(uint8(num(a, "copyKey")))
	case "setClipboard":
		return EncodeSetClipboard(num(a, "sequence"), b("paste"), str("text"))
	case "setDisplayPower":
		return EncodeSetDisplayPower(b("on"))
	case "rotateDevice":
		return EncodeEmpty(RotateDevice)
	case "uhidCreate":
		return EncodeUhidCreate(uint16(num(a, "id")), uint16(num(a, "vendorId")), uint16(num(a, "productId")), str("name"), bytesArg(a, "reportDesc"))
	case "uhidInput":
		return EncodeUhidInput(uint16(num(a, "id")), bytesArg(a, "data"))
	case "uhidDestroy":
		return EncodeUhidDestroy(uint16(num(a, "id")))
	case "openHardKeyboardSettings":
		return EncodeEmpty(OpenHardKeyboardSettings)
	case "startApp":
		return EncodeStartApp(str("name"))
	case "resetVideo":
		return EncodeEmpty(ResetVideo)
	case "cameraSetTorch":
		return EncodeCameraSetTorch(b("on"))
	case "cameraZoomIn":
		return EncodeEmpty(CameraZoomIn)
	case "cameraZoomOut":
		return EncodeEmpty(CameraZoomOut)
	case "resizeDisplay":
		return EncodeResizeDisplay(uint16(num(a, "width")), uint16(num(a, "height")))
	case "scanFile":
		return EncodeScanFile(str("path"))
	}
	return nil
}

func TestControlGolden(t *testing.T) {
	g := loadGolden(t)
	if len(g.Control) != 23 {
		t.Fatalf("expected 23 control vectors, got %d", len(g.Control))
	}
	for _, v := range AllVersions() {
		spec, _ := Lookup(v, false)
		for _, c := range g.Control {
			want, _ := hex.DecodeString(c.Hex)
			got := encode(c.Fn, c.Args)
			if !bytes.Equal(got, want) {
				t.Errorf("%s: encode\n got %x\nwant %x", c.Name, got, want)
			}
			supported := Compare(v, c.Since) >= 0
			err := spec.Validate(want)
			if supported && err != nil {
				t.Errorf("%s on %s: %v", c.Name, v, err)
			}
			if !supported && err == nil {
				t.Errorf("%s on %s: should be rejected", c.Name, v)
			}
			if supported {
				// Truncated messages are incomplete, not valid.
				if len(want) > 1 {
					if err := spec.Validate(want[:len(want)-1]); err == nil {
						t.Errorf("%s: truncated message validated", c.Name)
					}
				}
				if err := spec.Validate(append(append([]byte{}, want...), 0)); err == nil {
					t.Errorf("%s: trailing byte validated", c.Name)
				}
			}
		}
	}
}

func TestDeviceGolden(t *testing.T) {
	g := loadGolden(t)
	spec, _ := Lookup("5.0.1", false)
	var stream []byte
	for _, d := range g.Device {
		b, _ := hex.DecodeString(d.Hex)
		stream = append(stream, b...)
	}
	r := NewDeviceReader(bytes.NewReader(stream), spec)
	for _, d := range g.Device {
		m, err := r.Next()
		if err != nil {
			t.Fatal(d.Name, err)
		}
		want, _ := hex.DecodeString(d.Hex)
		if !bytes.Equal(m.Raw, want) || m.Type != d.Type {
			t.Errorf("%s: raw %x", d.Name, m.Raw)
		}
		switch d.Type {
		case DevClipboard:
			if m.Text != d.Text {
				t.Errorf("clipboard %q", m.Text)
			}
			if !bytes.Equal(EncodeDeviceClipboard(d.Text), want) {
				t.Error("encode clipboard")
			}
		case DevAckClipboard:
			if m.Sequence != 0x0102030405060708 || !bytes.Equal(EncodeDeviceAckClipboard(m.Sequence), want) {
				t.Errorf("ack %x", m.Sequence)
			}
		case DevUhidOutput:
			if m.ID != 42 || !bytes.Equal(m.Data, d.Data) || !bytes.Equal(EncodeDeviceUhidOutput(42, d.Data), want) {
				t.Errorf("uhid %+v", m)
			}
		}
	}
	// Big clipboard at the limit.
	big := EncodeDeviceClipboard(strings.Repeat("a", DeviceClipboardMaxLength))
	if len(big) != MessageMaxSize {
		t.Fatal(len(big))
	}
	m, err := NewDeviceReader(bytes.NewReader(big), spec).Next()
	if err != nil || len(m.Text) != DeviceClipboardMaxLength {
		t.Fatal(err)
	}
}

func TestStreamGolden(t *testing.T) {
	g := loadGolden(t)
	for _, s := range g.Session {
		spec, _ := Lookup(s.Version, false)
		got := spec.EncodeSession(SessionPacket{Width: s.Width, Height: s.Height, ClientResized: s.ClientResized})
		if hex.EncodeToString(got) != s.Hex {
			t.Errorf("%s: %x want %s", s.Name, got, s.Hex)
		}
	}
	for _, m := range g.Media {
		got := EncodeMediaHeader(m.Pts, m.Config, m.Key, m.Size)
		if hex.EncodeToString(got) != m.Hex {
			t.Errorf("%s: %x want %s", m.Name, got, m.Hex)
		}
	}
}

func TestLongTextTruncated(t *testing.T) {
	b := EncodeInjectText(strings.Repeat("a", 400))
	if len(b) != 5+InjectTextMaxLength || b[3] != 0x01 || b[4] != 0x2c {
		t.Fatalf("len %d % x", len(b), b[:5])
	}
	b = EncodeSetClipboard(1, false, strings.Repeat("a", MessageMaxSize))
	if len(b) != MessageMaxSize {
		t.Fatal(len(b))
	}
}

func TestLookup(t *testing.T) {
	for _, v := range []string{"4.0", "4.1", "5.0", "5.0.1"} {
		s, err := Lookup(v, false)
		if err != nil || !s.Tested || s.Version != v || s.MinSdk != 21 {
			t.Errorf("%s: %+v %v", v, s, err)
		}
	}
	if s, _ := Lookup("4.0", false); s.Profile != "4.x" || s.Handshake.ResizedFlag {
		t.Error("4.0 profile")
	}
	if s, _ := Lookup("5.0", false); s.Profile != "5.0" || !s.Handshake.ResizedFlag {
		t.Error("5.0 profile")
	}
	// 4.1 and 5.0 are wire-identical.
	a, _ := Lookup("4.1", false)
	b, _ := Lookup("5.0", false)
	if len(a.ControlTypes) != len(b.ControlTypes) || len(a.Options) != len(b.Options) || a.Handshake != b.Handshake {
		t.Error("4.1 and 5.0 should share the implementation")
	}
	_, err := Lookup("4.2", false)
	if ue, ok := err.(*UntestedError); !ok || ue.Profile != "4.x" || ue.HTTPStatus() != 409 {
		t.Errorf("4.2: %v", err)
	}
	s, err := Lookup("4.2", true)
	if err != nil || s.Tested || s.Version != "4.2" || s.Profile != "4.x" {
		t.Errorf("4.2 untested: %+v %v", s, err)
	}
	if _, err := Lookup("3.3.4", true); err == nil {
		t.Error("unknown major must be refused even with allow-untested")
	}
	if _, err := Lookup("x", false); err == nil {
		t.Error("bad version")
	}
	if ByName("5.0").Newest() != "5.0.1" || ByName("4.x").Newest() != "4.1" {
		t.Error("newest")
	}
}

func TestCompare(t *testing.T) {
	if Compare("5.0.1", "5.0") <= 0 || Compare("4.1", "5.0") >= 0 || Compare("5.0-rc1", "5.0") >= 0 || Compare("4.10", "4.9") <= 0 {
		t.Error("compare")
	}
}

func TestOptions(t *testing.T) {
	s40, _ := Lookup("4.0", false)
	s50, _ := Lookup("5.0", false)
	if err := s50.CheckOption("ignore_video_encoder_constraints", "true"); err != nil {
		t.Error(err)
	}
	if err := s40.CheckOption("ignore_video_encoder_constraints", "true"); err == nil {
		t.Error("4.0 lacks ignore_video_encoder_constraints")
	}
	for _, bad := range [][2]string{{"max_size", "abc"}, {"audio", "yes"}, {"crop", "1;rm -rf"}, {"scid", "1"}, {"tunnel_forward", "false"}, {"raw_stream", "true"}, {"cleanup", "false"}} {
		if err := s50.CheckOption(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := s50.CheckOption("crop", "1080:1920:0:0"); err != nil {
		t.Error(err)
	}
}
