// Package profile describes the mirroring wire protocol (the scrcpy server
// protocol) per server version.
//
// The tables come from diffing doc/develop.md and the server sources
// (Options, ControlMessage, ControlMessageReader, DeviceMessageWriter,
// Streamer, DesktopConnection, VideoCodec) across the scrcpy tags v4.0,
// v4.1, v5.0 and v5.0.1:
//
//   - v4.0 → v4.1 adds control message 22 (SCAN_FILE), the vp8 and vp9 video
//     codecs, the ignore_video_encoder_constraints launch option and the
//     "client resized" bit (LSB of the session packet's first u32).
//   - v4.1 → v5.0 → v5.0.1 changes nothing on the wire (5.0's changes are
//     host-side GPU decoding, audio capture usages and camera handling).
//   - minSdkVersion is 21 for every version.
//
// So 4.1 and 5.0 share one implementation; they stay separate allowlist
// entries, and 4.0 is the same implementation minus the 4.1 additions.
package profile

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// OptKind is the type of a launch option value.
type OptKind int

const (
	Bool OptKind = iota
	Int
	Float
	String
)

// Handshake describes the connection preamble.
type Handshake struct {
	DummyByte     bool `json:"dummyByte"`     // forward tunnel: 1 byte on the first socket
	DeviceNameLen int  `json:"deviceNameLen"` // device meta: fixed-size NUL-padded name
	CodecIDLen    int  `json:"codecIdLen"`    // u32 codec id on video/audio sockets
	SessionPacket bool `json:"sessionPacket"` // 12-byte video session packets (size)
	ResizedFlag   bool `json:"resizedFlag"`   // session packet carries "client resized"
	FrameHeader   int  `json:"frameHeader"`   // media packet header: u64 pts/flags + u32 size
}

// Spec is the protocol for one exact server version.
type Spec struct {
	Version   string             `json:"version"`
	Profile   string             `json:"profile"` // "4.x" or "5.0"
	Tested    bool               `json:"tested"`  // in the exact-version allowlist
	MinSdk    int                `json:"minSdk"`
	Handshake Handshake          `json:"handshake"`
	Options   map[string]OptKind `json:"-"`
	// ControlTypes are the accepted control message type ids.
	ControlTypes map[byte]string `json:"-"`
	// DeviceMessages are the device→client message type ids.
	DeviceMessages map[byte]string `json:"-"`
	VideoCodecs    []string        `json:"videoCodecs"`
	AudioCodecs    []string        `json:"audioCodecs"`
}

// Profile groups the versions of one protocol family.
type Profile struct {
	Name     string
	Major    int
	MinSdk   int
	Versions map[string]*Spec // exact allowlist
}

// Control message ids (shared by every known profile).
const (
	InjectKeycode            = 0
	InjectText               = 1
	InjectTouchEvent         = 2
	InjectScrollEvent        = 3
	BackOrScreenOn           = 4
	ExpandNotificationPanel  = 5
	ExpandSettingsPanel      = 6
	CollapsePanels           = 7
	GetClipboard             = 8
	SetClipboard             = 9
	SetDisplayPower          = 10
	RotateDevice             = 11
	UhidCreate               = 12
	UhidInput                = 13
	UhidDestroy              = 14
	OpenHardKeyboardSettings = 15
	StartApp                 = 16
	ResetVideo               = 17
	CameraSetTorch           = 18
	CameraZoomIn             = 19
	CameraZoomOut            = 20
	ResizeDisplay            = 21
	ScanFile                 = 22 // 4.1+
)

// Device message ids.
const (
	DevClipboard    = 0
	DevAckClipboard = 1
	DevUhidOutput   = 2
)

// Limits from ControlMessageReader / DeviceMessageWriter.
const (
	MessageMaxSize           = 1 << 18
	InjectTextMaxLength      = 300
	ClipboardTextMaxLength   = MessageMaxSize - 14
	DeviceClipboardMaxLength = MessageMaxSize - 5
)

var controlNames40 = map[byte]string{
	0: "inject_keycode", 1: "inject_text", 2: "inject_touch_event", 3: "inject_scroll_event",
	4: "back_or_screen_on", 5: "expand_notification_panel", 6: "expand_settings_panel",
	7: "collapse_panels", 8: "get_clipboard", 9: "set_clipboard", 10: "set_display_power",
	11: "rotate_device", 12: "uhid_create", 13: "uhid_input", 14: "uhid_destroy",
	15: "open_hard_keyboard_settings", 16: "start_app", 17: "reset_video",
	18: "camera_set_torch", 19: "camera_zoom_in", 20: "camera_zoom_out", 21: "resize_display",
}

var deviceNames = map[byte]string{0: "clipboard", 1: "ack_clipboard", 2: "uhid_output"}

// Launch options accepted from clients (Options.java). Options htadb sets
// itself (scid, tunnel_forward, cleanup, send_*, raw_stream, list_*) are
// not listed and are rejected if a client passes them.
var options40 = map[string]OptKind{
	"log_level": String, "video": Bool, "audio": Bool, "video_codec": String, "audio_codec": String,
	"video_source": String, "audio_source": String, "audio_dup": Bool, "max_size": Int,
	"min_size_alignment": Int, "video_bit_rate": Int, "audio_bit_rate": Int, "max_fps": Float,
	"angle": Float, "crop": String, "control": Bool, "display_id": Int, "show_touches": Bool,
	"stay_awake": Bool, "screen_off_timeout": Int, "video_codec_options": String,
	"audio_codec_options": String, "video_encoder": String, "audio_encoder": String,
	"power_off_on_close": Bool, "clipboard_autosync": Bool, "downsize_on_error": Bool,
	"power_on": Bool, "camera_id": String, "camera_size": String, "camera_facing": String,
	"camera_ar": String, "camera_zoom": Float, "camera_fps": Int, "camera_high_speed": Bool,
	"camera_torch": Bool, "new_display": String, "vd_destroy_content": Bool,
	"vd_system_decorations": Bool, "flex_display": Bool, "capture_orientation": String,
	"display_ime_policy": String, "keep_active": Bool,
}

func copyMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func spec40(version, prof string) *Spec {
	return &Spec{
		Version: version, Profile: prof, Tested: true, MinSdk: 21,
		Handshake:      Handshake{DummyByte: true, DeviceNameLen: 64, CodecIDLen: 4, SessionPacket: true, FrameHeader: 12},
		Options:        copyMap(options40),
		ControlTypes:   copyMap(controlNames40),
		DeviceMessages: copyMap(deviceNames),
		VideoCodecs:    []string{"h264", "h265", "av1"},
		AudioCodecs:    []string{"opus", "aac", "flac", "raw"},
	}
}

func spec41(version, prof string) *Spec {
	s := spec40(version, prof)
	s.ControlTypes[ScanFile] = "scan_file"
	s.Options["ignore_video_encoder_constraints"] = Bool
	s.VideoCodecs = append(s.VideoCodecs, "vp8", "vp9")
	s.Handshake.ResizedFlag = true
	return s
}

// Profiles is the registry, newest first.
var Profiles = []*Profile{
	{Name: "5.0", Major: 5, MinSdk: 21, Versions: map[string]*Spec{
		"5.0":   spec41("5.0", "5.0"),
		"5.0.1": spec41("5.0.1", "5.0"),
	}},
	{Name: "4.x", Major: 4, MinSdk: 21, Versions: map[string]*Spec{
		"4.0": spec40("4.0", "4.x"),
		"4.1": spec41("4.1", "4.x"),
	}},
}

// DefaultProfile is used when htadb picks the version itself.
const DefaultProfile = "5.0"

// FallbackProfile is where automatic fallback goes.
const FallbackProfile = "4.x"

var versionRE = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?(?:[-+]([0-9A-Za-z.]+))?$`)

// ValidVersion reports whether v looks like a server version.
func ValidVersion(v string) bool { return versionRE.MatchString(v) }

// Compare orders versions numerically (pre-release suffixes sort before the
// plain release).
func Compare(a, b string) int {
	ma, mb := versionRE.FindStringSubmatch(a), versionRE.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return strings.Compare(a, b)
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case ma[4] == mb[4]:
		return 0
	case ma[4] == "":
		return 1
	case mb[4] == "":
		return -1
	}
	return strings.Compare(ma[4], mb[4])
}

// Major returns the major version number.
func Major(v string) int {
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ByName returns a profile.
func ByName(name string) *Profile {
	for _, p := range Profiles {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// ForMajor returns the profile for a major version.
func ForMajor(major int) *Profile {
	for _, p := range Profiles {
		if p.Major == major {
			return p
		}
	}
	return nil
}

// Newest returns the newest allowlisted version of the profile.
func (p *Profile) Newest() string {
	var vs []string
	for v := range p.Versions {
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return Compare(vs[i], vs[j]) > 0 })
	return vs[0]
}

// Contains reports whether version v belongs to this profile's major.
func (p *Profile) Contains(v string) bool { return Major(v) == p.Major }

// UntestedError is returned for versions outside the allowlist (HTTP 409).
type UntestedError struct {
	Version string
	Profile string // "" when the major is unknown
}

func (e *UntestedError) Error() string {
	if e.Profile == "" {
		return fmt.Sprintf("mirroring server version %s is not supported (known profiles: %s)", e.Version, knownList())
	}
	return fmt.Sprintf("mirroring server version %s is not in the tested allowlist for profile %s (use --mirror-allow-untested to try it)", e.Version, e.Profile)
}

func (e *UntestedError) HTTPStatus() int   { return 409 }
func (e *UntestedError) ErrorCode() string { return "incompatible_version" }

func knownList() string {
	var out []string
	for _, p := range Profiles {
		var vs []string
		for v := range p.Versions {
			vs = append(vs, v)
		}
		sort.Slice(vs, func(i, j int) bool { return Compare(vs[i], vs[j]) < 0 })
		out = append(out, p.Name+" ("+strings.Join(vs, ", ")+")")
	}
	return strings.Join(out, "; ")
}

// Lookup returns the spec for an exact version. Unknown versions of a known
// major are refused unless allowUntested, in which case the major's newest
// spec is used with the requested version string and Tested=false.
func Lookup(v string, allowUntested bool) (*Spec, error) {
	if !ValidVersion(v) {
		return nil, fmt.Errorf("invalid mirroring server version %q", v)
	}
	p := ForMajor(Major(v))
	if p == nil {
		return nil, &UntestedError{Version: v}
	}
	if s, ok := p.Versions[v]; ok {
		return s, nil
	}
	if !allowUntested {
		return nil, &UntestedError{Version: v, Profile: p.Name}
	}
	base := p.Versions[p.Newest()]
	cp := *base
	cp.Version = v
	cp.Tested = false
	return &cp, nil
}

// AllVersions lists every allowlisted version, newest first.
func AllVersions() []string {
	var vs []string
	for _, p := range Profiles {
		for v := range p.Versions {
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return Compare(vs[i], vs[j]) > 0 })
	return vs
}

var safeValue = regexp.MustCompile(`^[0-9A-Za-z_.,:=/@+-]*$`)

// CheckOption validates a client-supplied launch option.
func (s *Spec) CheckOption(name, value string) error {
	k, ok := s.Options[name]
	if !ok {
		return fmt.Errorf("launch option %q is not accepted by server %s", name, s.Version)
	}
	switch k {
	case Bool:
		if value != "true" && value != "false" {
			return fmt.Errorf("option %s: want true or false", name)
		}
	case Int:
		if _, err := strconv.ParseInt(value, 10, 32); err != nil {
			return fmt.Errorf("option %s: want an integer", name)
		}
	case Float:
		if _, err := strconv.ParseFloat(value, 32); err != nil {
			return fmt.Errorf("option %s: want a number", name)
		}
	case String:
		if !safeValue.MatchString(value) {
			return fmt.Errorf("option %s: value contains unsupported characters", name)
		}
	}
	return nil
}

// ControlTypeList returns the accepted control ids, sorted.
func (s *Spec) ControlTypeList() []int {
	var out []int
	for k := range s.ControlTypes {
		out = append(out, int(k))
	}
	sort.Ints(out)
	return out
}
