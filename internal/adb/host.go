package adb

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ServerVersion returns the adb server's protocol version (host:version).
func (c *Client) ServerVersion(ctx context.Context) (int, error) {
	s, err := c.HostQuery(ctx, "host:version")
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 16, 32)
	if err != nil {
		return 0, fmt.Errorf("adb: bad version %q", s)
	}
	return int(v), nil
}

// Device is one line of `adb devices -l`.
type Device struct {
	Serial      string            `json:"serial"`
	State       string            `json:"state"`
	Product     string            `json:"product,omitempty"`
	Model       string            `json:"model,omitempty"`
	DeviceName  string            `json:"device,omitempty"`
	TransportID string            `json:"transportId,omitempty"`
	USB         string            `json:"usb,omitempty"`
	Extra       map[string]string `json:"extra,omitempty"`
}

// ParseDevices parses host:devices-l output.
func ParseDevices(s string) []Device {
	var out []Device
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		d := Device{Serial: f[0], State: f[1]}
		rest := f[2:]
		// States such as "no permissions (...)" contain spaces; anything
		// without a colon before the first key:value belongs to the state.
		i := 0
		for i < len(rest) && !strings.Contains(rest[i], ":") {
			d.State += " " + rest[i]
			i++
		}
		for _, kv := range rest[i:] {
			k, v, ok := strings.Cut(kv, ":")
			if !ok {
				continue
			}
			switch k {
			case "product":
				d.Product = v
			case "model":
				d.Model = v
			case "device":
				d.DeviceName = v
			case "transport_id":
				d.TransportID = v
			case "usb":
				d.USB = v
			default:
				if d.Extra == nil {
					d.Extra = map[string]string{}
				}
				d.Extra[k] = v
			}
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

// Devices lists attached devices.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	s, err := c.HostQuery(ctx, "host:devices-l")
	if err != nil {
		return nil, err
	}
	return ParseDevices(s), nil
}

// DeviceEvent is a change reported by TrackDevices.
type DeviceEvent struct {
	Type   string `json:"type"` // "add", "remove", "state", "change"
	Device Device `json:"device"`
	Old    string `json:"oldState,omitempty"`
}

// DiffDevices returns the events that turn prev into next.
func DiffDevices(prev, next []Device) []DeviceEvent {
	pm := map[string]Device{}
	for _, d := range prev {
		pm[d.Serial] = d
	}
	nm := map[string]bool{}
	var ev []DeviceEvent
	for _, d := range next {
		nm[d.Serial] = true
		o, ok := pm[d.Serial]
		switch {
		case !ok:
			ev = append(ev, DeviceEvent{Type: "add", Device: d})
		case o.State != d.State:
			ev = append(ev, DeviceEvent{Type: "state", Device: d, Old: o.State})
		case o.Model != d.Model || o.Product != d.Product || o.TransportID != d.TransportID:
			ev = append(ev, DeviceEvent{Type: "change", Device: d})
		}
	}
	for _, d := range prev {
		if !nm[d.Serial] {
			ev = append(ev, DeviceEvent{Type: "remove", Device: d})
		}
	}
	return ev
}

// TrackDevices streams the full device list every time it changes
// (host:track-devices-l). fn is called with each list; tracking stops when
// ctx ends, fn returns an error, or the connection breaks.
func (c *Client) TrackDevices(ctx context.Context, fn func([]Device) error) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Request("host:track-devices-l"); err != nil {
		return err
	}
	for {
		s, err := conn.ReadHexString()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == io.EOF {
				return fmt.Errorf("adb: device tracking closed")
			}
			return err
		}
		if err := fn(ParseDevices(s)); err != nil {
			return err
		}
	}
}

// Features returns the device's feature set (host-serial:<s>:features).
func (c *Client) Features(ctx context.Context, serial string) (map[string]bool, error) {
	s, err := c.HostQuery(ctx, "host-serial:"+serial+":features")
	if err != nil {
		return nil, err
	}
	f := map[string]bool{}
	for _, x := range strings.Split(strings.TrimSpace(s), ",") {
		if x != "" {
			f[x] = true
		}
	}
	return f, nil
}

// Connect runs host:connect:<addr>; adb reports failures in the message, so
// the text is returned along with an error when it does not look successful.
func (c *Client) Connect(ctx context.Context, addr string) (string, error) {
	s, err := c.HostQuery(ctx, "host:connect:"+addr)
	if err != nil {
		return "", err
	}
	if strings.Contains(s, "connected to") {
		return s, nil
	}
	return s, &Error{Service: "host:connect", Msg: s}
}

// Disconnect runs host:disconnect:<addr> ("" disconnects everything).
func (c *Client) Disconnect(ctx context.Context, addr string) (string, error) {
	return c.HostQuery(ctx, "host:disconnect:"+addr)
}

// Forward is one port forwarding (or reverse) rule.
type Forward struct {
	Serial string `json:"serial,omitempty"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

func parseForwards(s string, withSerial bool) []Forward {
	var out []Forward
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		switch {
		case withSerial && len(f) >= 3:
			out = append(out, Forward{Serial: f[0], Local: f[1], Remote: f[2]})
		case !withSerial && len(f) >= 3:
			// reverse:list-forward prints "<transport> <remote> <local>".
			out = append(out, Forward{Local: f[1], Remote: f[2]})
		case !withSerial && len(f) == 2:
			out = append(out, Forward{Local: f[0], Remote: f[1]})
		}
	}
	return out
}

// twoStatus handles services that answer OKAY (accepted) then OKAY/FAIL
// (result), optionally followed by a length-prefixed string (an allocated
// port for tcp:0).
func twoStatus(conn *Conn, service string, readResult bool) (string, error) {
	if err := conn.Request(service); err != nil {
		return "", err
	}
	if err := conn.Status(service); err != nil {
		return "", err
	}
	if !readResult {
		return "", nil
	}
	s, err := conn.ReadHexString()
	if err == io.EOF {
		return "", nil
	}
	return s, err
}

// ListForwards lists forwards for serial ("" = all devices).
func (c *Client) ListForwards(ctx context.Context, serial string) ([]Forward, error) {
	s, err := c.HostQuery(ctx, "host:list-forward")
	if err != nil {
		return nil, err
	}
	all := parseForwards(s, true)
	if serial == "" {
		return all, nil
	}
	var out []Forward
	for _, f := range all {
		if f.Serial == serial {
			out = append(out, f)
		}
	}
	return out, nil
}

// AddForward creates local→remote on the device; returns the allocated port
// when local is tcp:0.
func (c *Client) AddForward(ctx context.Context, serial, local, remote string, noRebind bool) (string, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	svc := "host-serial:" + serial + ":forward:"
	if noRebind {
		svc += "norebind:"
	}
	return twoStatus(conn, svc+local+";"+remote, local == "tcp:0")
}

// RemoveForward removes one forward, or all for the device when local is "".
func (c *Client) RemoveForward(ctx context.Context, serial, local string) error {
	conn, err := c.Dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	svc := "host-serial:" + serial + ":killforward-all"
	if local != "" {
		svc = "host-serial:" + serial + ":killforward:" + local
	}
	_, err = twoStatus(conn, svc, false)
	return err
}

// ListReverses lists reverse forwards on the device.
func (c *Client) ListReverses(ctx context.Context, serial string) ([]Forward, error) {
	conn, err := c.Transport(ctx, serial, "reverse:list-forward")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	s, err := conn.ReadHexString()
	if err != nil && err != io.EOF {
		return nil, err
	}
	return parseForwards(s, false), nil
}

func (c *Client) reverseCmd(ctx context.Context, serial, svc string, readResult bool) (string, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.Request("host:transport:" + serial); err != nil {
		return "", err
	}
	return twoStatus(conn, svc, readResult)
}

// AddReverse creates device remote → host local.
func (c *Client) AddReverse(ctx context.Context, serial, remote, local string, noRebind bool) (string, error) {
	svc := "reverse:forward:"
	if noRebind {
		svc += "norebind:"
	}
	return c.reverseCmd(ctx, serial, svc+remote+";"+local, remote == "tcp:0")
}

// RemoveReverse removes one reverse forward, or all when remote is "".
func (c *Client) RemoveReverse(ctx context.Context, serial, remote string) error {
	svc := "reverse:killforward-all"
	if remote != "" {
		svc = "reverse:killforward:" + remote
	}
	_, err := c.reverseCmd(ctx, serial, svc, false)
	return err
}

// Reboot reboots into target ("", "bootloader", "recovery", "sideload", ...).
func (c *Client) Reboot(ctx context.Context, serial, target string) error {
	conn, err := c.Transport(ctx, serial, "reboot:"+target)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, _ = io.Copy(io.Discard, conn)
	return nil
}

// Root, Unroot and Remount run the matching adbd services and return their
// text output.
func (c *Client) Root(ctx context.Context, serial string) (string, error) {
	b, err := c.DeviceOutput(ctx, serial, "root:")
	return string(b), err
}

func (c *Client) Unroot(ctx context.Context, serial string) (string, error) {
	b, err := c.DeviceOutput(ctx, serial, "unroot:")
	return string(b), err
}

func (c *Client) Remount(ctx context.Context, serial string) (string, error) {
	b, err := c.DeviceOutput(ctx, serial, "remount:")
	return string(b), err
}

// LocalAbstract opens a raw stream to the device's abstract unix socket.
func (c *Client) LocalAbstract(ctx context.Context, serial, name string) (*Conn, error) {
	return c.Transport(ctx, serial, "localabstract:"+name)
}
